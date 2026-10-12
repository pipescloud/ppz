package server

import (
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/clock"
	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/natsauth"
	"github.com/pipescloud/ppz/internal/natsubj"
)

func (s *Server) handleAuthExchange(w http.ResponseWriter, r *http.Request) {
	var req cliproto.AuthExchangeRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, cliproto.New(cliproto.EInvalidAPIKey))
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()

	// Resolve the credential into an org. Two shapes:
	//   ppz_oauth_<…> → OAuth bearer; org defaults to caller's first
	//                    owned org. If req.AccountID is set, validate the
	//                    user is a member of that org and use it.
	//   ppz_<…>       → V1 API key; org = key.AccountID. req.AccountID
	//                    must match (or be empty) — API keys are
	//                    org-scoped at issuance.
	var accountID uuid.UUID
	// The principal the credential is minted for — the subject ACL
	// grants name, not whoever minted the key.
	var callerPrincipal uuid.UUID
	if strings.HasPrefix(req.APIKey, bearerPrefixOAuth) {
		tok, err := db.LookupBearerToken(ctx, s.Pool, req.APIKey)
		if err != nil {
			writeErr(w, cliproto.New(cliproto.EInvalidAPIKey))
			return
		}
		if req.AccountID != "" {
			parsed, err := uuid.Parse(req.AccountID)
			if err != nil {
				writeErr(w, &cliproto.Error{Code: "E_INVALID_ORG", Message: "org_id is not a valid uuid"})
				return
			}
			if !db.IsMemberOrOwner(ctx, s.Pool, parsed, tok.UserID) {
				writeErr(w, &cliproto.Error{Code: "E_INVALID_ORG", Message: "user is not a member of org"})
				return
			}
			accountID = parsed
			callerPrincipal = tok.UserID
		} else {
			defaultOrg, err := db.DefaultAccountFor(ctx, s.Pool, tok.UserID)
			if err != nil {
				writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "user belongs to no org"})
				return
			}
			accountID = defaultOrg.ID
			callerPrincipal = tok.UserID
		}
	} else {
		key, err := db.LookupAPIKey(ctx, s.Pool, req.APIKey)
		if err != nil {
			writeErr(w, cliproto.New(cliproto.EInvalidAPIKey))
			return
		}
		accountID = key.AccountID
		callerPrincipal = key.Actor()
		if req.AccountID != "" && req.AccountID != accountID.String() {
			writeErr(w, &cliproto.Error{Code: "E_INVALID_ORG", Message: "api key is not for this org"})
			return
		}
	}

	// Org name is surfaced in `ppz status` so users see "alpha" instead
	// of a UUID. Looking it up here keeps it on the same round-trip as
	// the auth result; the daemon caches it alongside Credentials.
	org, err := db.GetAccount(ctx, s.Pool, accountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "org lookup: " + err.Error()})
		return
	}

	// Hand back a NATS URL that matches how the client reached us:
	// - host client hit http://localhost:8080 → nats://localhost:4222
	// - in-compose client hit http://ppz-server:8080 → nats://ppz-server:4222
	// PPZ_NATS_PUBLIC_URL (if set) wins over derivation, for ops overrides.
	natsURL := s.NATSURL
	if natsURL == "" {
		host := r.Host // includes port
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		natsURL = "nats://" + host + ":4222"
	}

	// Mint a fresh NATS user JWT for this caller's org — short-lived
	// (5min default). The daemon re-runs /auth/exchange before this
	// expires.
	//
	// Phase 3.5: signed by the org's per-org account signing key
	// (lazily provisioned by AccountPool). Tenant isolation is
	// enforced by NATS at the account boundary; the user JWT just
	// needs broad pub/sub within the account.
	natsUserJWTTTL := 5 * time.Minute
	if s.DevLogin {
		if v := os.Getenv("PPZ_NATS_JWT_TTL"); v != "" {
			if d, perr := time.ParseDuration(v); perr == nil {
				natsUserJWTTTL = d
			}
		}
	}
	oa, err := s.AccountPool.Get(ctx, accountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "provision org account: " + err.Error()})
		return
	}
	// ACL Phase 3: the credential is compiled from the caller's
	// effective access only when the org has opted in. Every org ships
	// with acl_enforced=false, so this keeps returning the wide-open
	// credential until an admin turns it on.
	aclEnforced, _ := db.ACLEnforced(ctx, s.Pool, accountID)
	perms, err := s.natsPermissionsFor(ctx, accountID, callerPrincipal, aclEnforced)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "resolve acl: " + err.Error()})
		return
	}
	natsJWT, natsSeed, err := natsauth.MintUserJWTWithPermissions(
		oa.AccountPub, oa.SigningKP,
		"ppz-user-"+accountID.String(),
		perms,
		clock.Now().Add(natsUserJWTTTL).Unix(),
	)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "mint nats user jwt: " + err.Error()})
		return
	}

	now := clock.Now()
	writeJSON(w, http.StatusOK, cliproto.AuthExchangeReply{
		JWT:          "stub-jwt-not-yet-issued",
		NATSURL:      natsURL,
		AccountID:    accountID.String(),
		AccountName:  org.Name,
		ExpiresAt:    now.Add(natsUserJWTTTL),
		ACLEnforced:  aclEnforced,
		NATSUserJWT:  natsJWT,
		NATSUserSeed: natsSeed,
	})
}

func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	var req cliproto.CreateSourceRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, cliproto.New(cliproto.EInvalidHandle))
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	src, e := s.createSource(ctx, key, req.Manifold, req.Handle, db.SourceKind(req.Kind))
	if e != nil {
		writeErr(w, e)
		return
	}
	writeJSON(w, http.StatusCreated, cliproto.CreateSourceReply{
		ID:        src.ID.String(),
		Handle:    src.Handle,
		Manifold:  src.Manifold,
		Kind:      string(src.Kind),
		Subject:   natsubj.BuildSubject(key.AccountID, src.Manifold, src.Handle, "inbox"),
		CreatedAt: src.CreatedAt,
	})
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	sources, err := db.ListSourcesForOrg(ctx, s.Pool, key.AccountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	// Walk all sources + their user-pipes once to collect every creator
	// id that needs a username for the response, then resolve in one
	// shot. Avoids N+1 user lookups on org pages with many sources.
	pipesBySource := make(map[uuid.UUID][]db.Pipe, len(sources))
	idSet := make(map[uuid.UUID]struct{})
	for _, src := range sources {
		idSet[src.CreatedByUserID] = struct{}{}
		userPipes, err := db.ListPipesForSource(ctx, s.Pool, src.ID)
		if err != nil {
			writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
			return
		}
		pipesBySource[src.ID] = userPipes
		for _, p := range userPipes {
			idSet[p.CreatedByUserID] = struct{}{}
		}
	}
	ids := make([]uuid.UUID, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	usernames, err := db.UsernamesByIDs(ctx, s.Pool, ids)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	out := make([]cliproto.Source, 0, len(sources))
	for _, src := range sources {
		userPipes := pipesBySource[src.ID]
		names := make([]string, 0, len(userPipes))
		pipeInfos := make([]cliproto.PipeInfo, 0, len(userPipes))
		for _, p := range userPipes {
			names = append(names, p.Name)
			pipeInfos = append(pipeInfos, cliproto.PipeInfo{
				Pipe:      p.Name,
				CreatedBy: usernames[p.CreatedByUserID],
			})
		}
		out = append(out, cliproto.Source{
			Handle:               src.Handle,
			Manifold:             src.Manifold,
			Kind:                 string(src.Kind),
			Pipes:                names,
			PipeInfos:            pipeInfos,
			CreatedBy:            usernames[src.CreatedByUserID],
			LastBroadcastAt:      src.LastBroadcastAt,
			LastBroadcastPayload: src.LastBroadcastPayload,
		})
	}
	writeJSON(w, http.StatusOK, cliproto.ListSourcesReply{Sources: out})
}

// handleCreatePipe: POST /api/v1/sources/{handle}/pipes
//
// handleEnsurePTY: POST /api/v1/sources/{handle}/ensure-pty
//
// Promotes an existing source to a full terminal, idempotently. Bare `ppz
// terminal share` runs against the session's current source, which may have
// been created inbox-only (kind=message, e.g. via `source create` or
// `connect`). Sharing a source declares it a terminal, so this endpoint flips
// its kind to pty and provisions the COMPLETE pty pipe set — including the
// reserved `system` (write-lease) and `inbox` pipes that the user pipe-create
// path refuses. Provisioning goes through ensureSourceStreams (trusted, no
// reserved-name gate), the same primitive source-creation uses — which also
// means a `ppz pipe set` on an auto-pipe survives the re-provision.
//
// Idempotent in both directions: a source already pty is left as-is (kind
// UPDATE is a harmless no-op change) and every stream ensure is
// create-if-absent, so repeated bare shares converge without error.
func (s *Server) handleEnsurePTY(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	handle := r.PathValue("handle")
	if err := natsubj.ValidateHandle(handle); err != nil {
		writeErr(w, cliproto.NewInvalidHandle(handle))
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()

	src, err := db.GetSourceByHandle(ctx, s.Pool, key.AccountID, handle)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, cliproto.NewSourceNotFound(handle))
			return
		}
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	// Promote kind → pty when not already. Skip the write when it's a no-op so
	// an already-pty source doesn't churn the row.
	if src.Kind != db.SourceKindPTY {
		if err := db.UpdateSourceKind(ctx, s.Pool, key.AccountID, handle, db.SourceKindPTY); err != nil {
			writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
			return
		}
		src.Kind = db.SourceKindPTY
	}

	// Provision the full pty pipe set via the trusted primitive (bypasses the
	// reserved-name gate, so `system`/`inbox` get created). Idempotent per
	// pipe, and override-aware: bare `ppz terminal share` runs this on every
	// invocation, so a defaults-only re-provision here would silently revert
	// any `pipe set` the user had made on those pipes.
	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "org account: " + err.Error()})
		return
	}
	if err := ensureSourceStreams(ctx, s.Pool, js, key.AccountID, src); err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	// The account's pipe set just changed, and a compiled credential
	// enumerates it. No-op when the org has not opted into enforcement.
	s.notifyACLChanged(ctx, key.AccountID)
	writeJSON(w, http.StatusOK, cliproto.CreateSourceReply{
		ID:       src.ID.String(),
		Handle:   src.Handle,
		Manifold: src.Manifold,
		Kind:     string(src.Kind),
		Subject:  natsubj.BuildSubject(key.AccountID, src.Manifold, src.Handle, "inbox"),
	})
}

// Body: cliproto.PipeCreateRequest. Validates pipe name (regex + reserved
// + not auto-provisioned), inserts the row with retention overrides
// (NULL → server default), provisions the JetStream stream with the
// resolved config, and returns the resolved retention so the caller
// prints exactly what got created.
func (s *Server) handleCreatePipe(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	var req cliproto.PipeCreateRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, cliproto.New(cliproto.EInvalidPipe))
		return
	}
	handle := r.PathValue("handle")
	if err := natsubj.ValidateHandle(handle); err != nil {
		writeErr(w, cliproto.NewInvalidHandle(handle))
		return
	}
	// Validated again inside createCollaredPipe; checked here first so a
	// bad name is reported ahead of a missing source.
	if e := validateUserPipe(req.Name); e != nil {
		writeErr(w, e)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()

	src, err := db.GetSourceByHandle(ctx, s.Pool, key.AccountID, handle)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, cliproto.NewSourceNotFound(handle))
			return
		}
		writeErr(w, internalErr(err.Error()))
		return
	}
	created, e := s.createCollaredPipe(ctx, key, src, req.Name, req.TTLSeconds, req.MaxMsgs, req.MaxBytes)
	if e != nil {
		writeErr(w, e)
		return
	}
	writeJSON(w, http.StatusCreated, pipeCreateReply(key, src.Manifold, src.Handle, created))
}

// handleCreatePipeFullPath: POST /api/v1/pipes
//
// The Phase 1.5 sourceless (uncollared) pipe creation endpoint. Body is
// cliproto.PipeCreateRequest with SourceHandle == nil and Handle == "".
// Collared pipes still flow through POST /api/v1/sources/{handle}/pipes
// — clients send a collared request there, an uncollared request here.
//
// Splits responsibility cleanly:
//   - Collared shortcut: existing endpoint, source row already known
//   - Uncollared (this): manifold + name, no source row
func (s *Server) handleCreatePipeFullPath(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	var req cliproto.PipeCreateRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, cliproto.New(cliproto.EInvalidPipe))
		return
	}
	if req.Handle != "" || (req.SourceHandle != nil && *req.SourceHandle != "") {
		writeErr(w, &cliproto.Error{Code: cliproto.EInvalidPipe, Message: "POST /api/v1/pipes is the uncollared (sourceless) endpoint; for collared pipes use POST /api/v1/sources/{handle}/pipes"})
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	created, e := s.createUncollaredPipe(ctx, key, req.Manifold, req.Name, req.TTLSeconds, req.MaxMsgs, req.MaxBytes)
	if e != nil {
		writeErr(w, e)
		return
	}
	writeJSON(w, http.StatusCreated, pipeCreateReply(key, req.Manifold, "", created))
}

func pipeCreateReply(key db.APIKey, manifold, handle string, c pipeCreated) cliproto.PipeCreateReply {
	return cliproto.PipeCreateReply{
		Handle:     handle,
		Manifold:   manifold,
		Name:       c.pipe.Name,
		StreamName: natsubj.BuildStreamName(key.AccountID, manifold, handle, c.pipe.Name),
		TTLSeconds: int(c.maxAge / time.Second),
		MaxMsgs:    c.maxMsgs,
		MaxBytes:   c.maxBytes,
	}
}

// handleListUncollaredPipes: GET /api/v1/pipes
//
// Returns every sourceless (source_id IS NULL) pipe row in the caller's
// account. The collared rows still come through /api/v1/sources joins;
// this endpoint surfaces what walking sources alone misses. Phase 1.5.
func (s *Server) handleListUncollaredPipes(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	rows, err := db.ListUncollaredPipesForAccount(ctx, s.Pool, key.AccountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	// Resolve creators in one batch. The CLI's `ppz ls` renders this in
	// the CREATOR column.
	idSet := make(map[uuid.UUID]struct{}, len(rows))
	for _, p := range rows {
		idSet[p.CreatedByUserID] = struct{}{}
	}
	ids := make([]uuid.UUID, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	creators, err := db.UsernamesByIDs(ctx, s.Pool, ids)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	out := make([]cliproto.UncollaredPipeListEntry, 0, len(rows))
	for _, p := range rows {
		out = append(out, cliproto.UncollaredPipeListEntry{
			Manifold:  p.Manifold,
			Name:      p.Name,
			CreatedBy: creators[p.CreatedByUserID],
		})
	}
	writeJSON(w, http.StatusOK, cliproto.ListUncollaredPipesReply{Pipes: out})
}

// handleDestroyUncollaredPipe: DELETE /api/v1/pipes?manifold=M&name=N
//
// Removes the uncollared pipe row + its JetStream stream. Idempotent on
// missing stream (row is the source of truth). Phase 1.5.
func (s *Server) handleDestroyUncollaredPipe(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	if e := s.destroyUncollaredPipe(ctx, key, r.URL.Query().Get("manifold"), r.URL.Query().Get("name")); e != nil {
		writeErr(w, e)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDestroySource: DELETE /api/v1/sources/{handle}
//
// Removes the source row (CASCADE removes pipe rows) then best-effort deletes
// all JetStream streams (auto-provisioned + user-created). Returns 204.
func (s *Server) handleDestroySource(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	handle := r.PathValue("handle")
	if err := natsubj.ValidateHandle(handle); err != nil {
		writeErr(w, cliproto.NewInvalidHandle(handle))
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()

	src, err := db.GetSourceByHandle(ctx, s.Pool, key.AccountID, handle)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, cliproto.NewSourceNotFound(handle))
			return
		}
		writeErr(w, internalErr(err.Error()))
		return
	}
	if e := s.destroySource(ctx, key, src); e != nil {
		writeErr(w, e)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDestroyPipe: DELETE /api/v1/sources/{handle}/pipes/{name}
//
// Removes the row + the JetStream stream. Returns 204 on success.
// Idempotent on missing stream (the row is the source of truth).
// handlePurgePipe: POST /api/v1/sources/{handle}/pipes/{name}/purge —
// empties the pipe's buffer (`ppz pipe purge H.NAME`). Admin on the pipe
// while the org enforces ACLs, like destroy and retention.
func (s *Server) handlePurgePipe(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	handle, name := r.PathValue("handle"), r.PathValue("name")
	if err := natsubj.ValidateHandle(handle); err != nil {
		writeErr(w, cliproto.NewInvalidHandle(handle))
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	src, err := db.GetSourceByHandle(ctx, s.Pool, key.AccountID, handle)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, cliproto.NewSourceNotFound(handle))
			return
		}
		writeErr(w, internalErr(err.Error()))
		return
	}
	if err := s.requirePipeAdmin(ctx, key, handle+"."+name); err != nil {
		writeErr(w, cliproto.New(cliproto.EPipeForbidden))
		return
	}
	n, e := s.purgeCollaredPipe(ctx, key, src, name)
	if e != nil {
		writeErr(w, e)
		return
	}
	writeJSON(w, http.StatusOK, cliproto.PipePurgeReply{Handle: src.Handle, Manifold: src.Manifold, Name: name, Purged: n})
}

// handlePurgeUncollaredPipe: POST /api/v1/pipes/purge?manifold=&name=.
func (s *Server) handlePurgeUncollaredPipe(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	manifold, name := r.URL.Query().Get("manifold"), r.URL.Query().Get("name")
	ctx, cancel := withTimeout(r)
	defer cancel()
	path := name
	if manifold != "" {
		path = manifold + "." + name
	}
	if err := s.requirePipeAdmin(ctx, key, path); err != nil {
		writeErr(w, cliproto.New(cliproto.EPipeForbidden))
		return
	}
	n, e := s.purgeUncollaredPipe(ctx, key, manifold, name)
	if e != nil {
		writeErr(w, e)
		return
	}
	writeJSON(w, http.StatusOK, cliproto.PipePurgeReply{Manifold: manifold, Name: name, Purged: n})
}

func (s *Server) handleDestroyPipe(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	handle := r.PathValue("handle")
	name := r.PathValue("name")
	if err := natsubj.ValidateHandle(handle); err != nil {
		writeErr(w, cliproto.NewInvalidHandle(handle))
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()

	src, err := db.GetSourceByHandle(ctx, s.Pool, key.AccountID, handle)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, cliproto.NewSourceNotFound(handle))
			return
		}
		writeErr(w, internalErr(err.Error()))
		return
	}
	if e := s.destroyCollaredPipe(ctx, key, src, name); e != nil {
		writeErr(w, e)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetSource(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	handle := r.PathValue("handle")
	ctx, cancel := withTimeout(r)
	defer cancel()
	src, err := db.GetSourceByHandle(ctx, s.Pool, key.AccountID, handle)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, cliproto.NewSourceNotFound(handle))
			return
		}
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cliproto.Source{
		Handle:               src.Handle,
		Kind:                 string(src.Kind),
		LastBroadcastAt:      src.LastBroadcastAt,
		LastBroadcastPayload: src.LastBroadcastPayload,
	})
}
