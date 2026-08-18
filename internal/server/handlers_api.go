package server

import (
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/clock"
	"github.com/pipescloud/ppz/internal/cliproto"
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
		} else {
			defaultOrg, err := db.DefaultAccountFor(ctx, s.Pool, tok.UserID)
			if err != nil {
				writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "user belongs to no org"})
				return
			}
			accountID = defaultOrg.ID
		}
	} else {
		key, err := db.LookupAPIKey(ctx, s.Pool, req.APIKey)
		if err != nil {
			writeErr(w, cliproto.New(cliproto.EInvalidAPIKey))
			return
		}
		accountID = key.AccountID
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
	natsJWT, natsSeed, err := natsauth.MintUserJWTInAccount(
		oa.AccountPub, oa.SigningKP,
		"ppz-user-"+accountID.String(),
		[]string{">"}, []string{">"},
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
		AccountID:        accountID.String(),
		AccountName:      org.Name,
		ExpiresAt:    now.Add(natsUserJWTTTL),
		NATSUserJWT:  natsJWT,
		NATSUserSeed: natsSeed,
	})
}

// handleRevokeKey marks an API key revoked. Idempotent: revoking an
// already-revoked key returns 200 (the desired state — revoked — is
// already in place). Missing keys return 404. No auth — same posture
// as the existing /orgs/<id>/keys POST that creates them.
func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid key id", http.StatusBadRequest)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	if err := db.RevokeAPIKey(ctx, s.Pool, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.Error(w, "key not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Browser form submit (has Referer) → redirect back so the user
	// sees the updated org page. API clients (curl, no Referer) get
	// a plain 200.
	if ref := r.Referer(); ref != "" {
		http.Redirect(w, r, ref, http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	var req cliproto.CreateSourceRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, cliproto.New(cliproto.EInvalidHandle))
		return
	}
	if err := natsubj.ValidateHandle(req.Handle); err != nil {
		writeErr(w, cliproto.NewInvalidHandle(req.Handle))
		return
	}
	if req.Manifold != "" {
		for _, seg := range strings.Split(req.Manifold, ".") {
			if err := natsubj.ValidateHandle(seg); err != nil {
				writeErr(w, &cliproto.Error{Code: cliproto.EInvalidManifold, Message: "manifold segment invalid: " + seg})
				return
			}
		}
	}
	ctx, cancel := withTimeout(r)
	defer cancel()

	kind := db.SourceKind(req.Kind)
	if kind == "" {
		kind = db.SourceKindMessage
	}
	if kind != db.SourceKindMessage && kind != db.SourceKindPTY {
		writeErr(w, &cliproto.Error{Code: "E_INVALID_KIND", Message: "kind must be 'message' or 'pty'"})
		return
	}

	// Phase 1.5.1 first-wins collision rule (both directions):
	// (a) reject if an uncollared pipe with this name already exists at
	//     this manifold;
	// (b) reject if there's already at least one pipe (collared or
	//     uncollared) under the manifold-prefix path the new source's
	//     auto-pipes would occupy — i.e. manifold==req.Manifold+"."+
	//     req.Handle (or req.Handle when req.Manifold is empty).
	if taken, err := db.UncollaredPipeExists(ctx, s.Pool, key.AccountID, req.Manifold, req.Handle); err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	} else if taken {
		writeErr(w, cliproto.NewNameTakenByUncollaredPipe(req.Handle, req.Manifold))
		return
	}
	reservedPath := req.Handle
	if req.Manifold != "" {
		reservedPath = req.Manifold + "." + req.Handle
	}
	if anyPipe, err := db.PipesExistAtManifold(ctx, s.Pool, key.AccountID, reservedPath); err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	} else if anyPipe {
		writeErr(w, cliproto.NewManifoldReservedBySource(reservedPath, req.Handle, req.Manifold))
		return
	}

	src, err := db.InsertSource(ctx, s.Pool, key.AccountID, key.CreatedByUserID, req.Manifold, req.Handle, kind)
	if err != nil {
		if errors.Is(err, db.ErrHandleTaken) {
			writeErr(w, cliproto.NewSourceTaken(req.Handle))
			return
		}
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "account: " + err.Error()})
		return
	}
	// Phase 1.5.1: auto-pipes provisioned at the source's manifold
	// via the four-role builder. For root-manifold sources (the
	// pre-1.5.1 case) the stream name is `pipe_<orgshort>_<handle>_<pipe>`
	// — a wire-format change from the legacy `source_…` prefix that
	// requires a Reset Database cutover.
	for _, p := range src.Pipes() {
		if err := ensurePipeStream(ctx, js, key.AccountID, src.Manifold, src.Handle, p); err != nil {
			writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
			return
		}
	}
	subject := natsubj.BuildSubject(key.AccountID, src.Manifold, src.Handle, "inbox")

	writeJSON(w, http.StatusCreated, cliproto.CreateSourceReply{
		ID:        src.ID.String(),
		Handle:    src.Handle,
		Manifold:  src.Manifold,
		Kind:      string(src.Kind),
		Subject:   subject,
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
// path refuses. Provisioning goes through ensurePipeStream (trusted, no
// reserved-name gate), the same primitive source-creation uses.
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
	// pipe.
	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "org account: " + err.Error()})
		return
	}
	for _, p := range src.Pipes() {
		if err := ensurePipeStream(ctx, js, key.AccountID, src.Manifold, src.Handle, p); err != nil {
			writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
			return
		}
	}

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
	// natsubj.ValidateUserPipeName returns either "invalid pipe name" (regex
	// rejection) or "name is reserved" — keep the distinction so the user
	// sees which one tripped.
	if err := natsubj.ValidateUserPipeName(req.Name); err != nil {
		if err.Error() == "name is reserved" {
			writeErr(w, cliproto.NewInvalidPipeReserved(req.Name))
		} else {
			writeErr(w, cliproto.NewInvalidPipeName(req.Name))
		}
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

	// Phase 1.5: this endpoint is the collared-pipe path (POST
	// /api/v1/sources/{handle}/pipes), so source_id is always set. The
	// pipe inherits the source's manifold (root '' until Cycle B adds
	// explicit manifold support).
	pipe, err := db.InsertPipe(ctx, s.Pool, key.AccountID, src.Manifold, &src.ID, key.CreatedByUserID, req.Name,
		req.TTLSeconds, req.MaxMsgs, req.MaxBytes)
	if err != nil {
		if errors.Is(err, db.ErrPipeNameTaken) {
			writeErr(w, cliproto.NewPipeTaken(req.Name, handle))
			return
		}
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	maxAge, maxMsgs, maxBytes := resolveRetention(pipeLayer(pipe))

	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "org account: " + err.Error()})
		return
	}
	if err := ensurePipeStreamWithRetention(ctx, js, key.AccountID,
		src.Manifold, src.Handle, pipe.Name, maxAge, maxMsgs, maxBytes); err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	s.auditPipe(ctx, key, db.AuditActionPipeCreate,
		cliproto.FormatPipePath(src.Manifold, src.Handle, pipe.Name),
		nil, snapshotRetention(maxAge, maxMsgs, maxBytes).mustJSON())

	writeJSON(w, http.StatusCreated, cliproto.PipeCreateReply{
		Handle:     src.Handle,
		Manifold:   src.Manifold,
		Name:       pipe.Name,
		StreamName: natsubj.BuildStreamName(key.AccountID, src.Manifold, src.Handle, pipe.Name),
		TTLSeconds: int(maxAge / time.Second),
		MaxMsgs:    maxMsgs,
		MaxBytes:   maxBytes,
	})
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
	if req.Manifold != "" {
		for _, seg := range strings.Split(req.Manifold, ".") {
			if err := natsubj.ValidateHandle(seg); err != nil {
				writeErr(w, &cliproto.Error{Code: cliproto.EInvalidManifold, Message: "manifold segment invalid: " + seg})
				return
			}
		}
	}
	if err := natsubj.ValidateUserPipeName(req.Name); err != nil {
		if err.Error() == "name is reserved" {
			writeErr(w, cliproto.NewInvalidPipeReserved(req.Name))
		} else {
			writeErr(w, cliproto.NewInvalidPipeName(req.Name))
		}
		return
	}

	ctx, cancel := withTimeout(r)
	defer cancel()

	// Phase 1.5.1 first-wins collision rule: reject if a source with the
	// same name already exists at this manifold.
	if exists, err := db.SourceExistsAtManifold(ctx, s.Pool, key.AccountID, req.Manifold, req.Name); err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	} else if exists {
		writeErr(w, cliproto.NewNameTakenBySource(req.Name, req.Manifold))
		return
	}
	// Phase 1.5.1 first-wins collision rule: also reject if the manifold
	// path itself collides with a reserved source-prefix. Source X at
	// manifold M reserves the prefix M.X via its auto-pipes — so any pipe
	// at manifold M.X (or M.X.<deeper>) would land on the source's
	// subjects. Walk the segments and check each prefix.
	if req.Manifold != "" {
		segs := strings.Split(req.Manifold, ".")
		for i, seg := range segs {
			parent := strings.Join(segs[:i], ".")
			reserved, err := db.SourceExistsAtManifold(ctx, s.Pool, key.AccountID, parent, seg)
			if err != nil {
				writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
				return
			}
			if reserved {
				prefix := strings.Join(segs[:i+1], ".")
				writeErr(w, cliproto.NewManifoldReservedBySource(prefix, seg, parent))
				return
			}
		}
	}

	pipe, err := db.InsertPipe(ctx, s.Pool, key.AccountID, req.Manifold, nil, key.CreatedByUserID, req.Name,
		req.TTLSeconds, req.MaxMsgs, req.MaxBytes)
	if err != nil {
		if errors.Is(err, db.ErrPipeNameTaken) {
			writeErr(w, cliproto.NewUncollaredPipeTaken(req.Name, req.Manifold))
			return
		}
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	maxAge, maxMsgs, maxBytes := resolveRetention(pipeLayer(pipe))

	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "account: " + err.Error()})
		return
	}
	if err := ensurePipeStreamWithRetention(ctx, js, key.AccountID, req.Manifold, "", pipe.Name, maxAge, maxMsgs, maxBytes); err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	s.auditPipe(ctx, key, db.AuditActionPipeCreate,
		cliproto.FormatPipePath(req.Manifold, "", pipe.Name),
		nil, snapshotRetention(maxAge, maxMsgs, maxBytes).mustJSON())

	writeJSON(w, http.StatusCreated, cliproto.PipeCreateReply{
		Handle:     "",
		Manifold:   req.Manifold,
		Name:       pipe.Name,
		StreamName: natsubj.BuildStreamName(key.AccountID, req.Manifold, "", pipe.Name),
		TTLSeconds: int(maxAge / time.Second),
		MaxMsgs:    maxMsgs,
		MaxBytes:   maxBytes,
	})
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
	manifold := r.URL.Query().Get("manifold")
	name := r.URL.Query().Get("name")
	if name == "" {
		writeErr(w, cliproto.New(cliproto.EInvalidPipe))
		return
	}
	if err := natsubj.ValidateUserPipeName(name); err != nil {
		writeErr(w, cliproto.NewInvalidPipeName(name))
		return
	}
	if manifold != "" {
		for _, seg := range strings.Split(manifold, ".") {
			if err := natsubj.ValidateHandle(seg); err != nil {
				writeErr(w, &cliproto.Error{Code: cliproto.EInvalidManifold, Message: "manifold segment invalid: " + seg})
				return
			}
		}
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	if err := db.DeleteUncollaredPipe(ctx, s.Pool, key.AccountID, manifold, name); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, cliproto.NewUncollaredPipeNotFound(name, manifold))
			return
		}
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "account: " + err.Error()})
		return
	}
	if err := deleteUncollaredPipeStream(ctx, js, key.AccountID, manifold, name); err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
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
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	// Snapshot user-created pipe names before CASCADE removes them.
	userPipes, err := db.ListPipesForSource(ctx, s.Pool, src.ID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	if err := db.DeleteSource(ctx, s.Pool, key.AccountID, handle); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, cliproto.NewSourceNotFound(handle))
			return
		}
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	// Stream cleanup is best-effort — the DB row is already gone so orphaned
	// streams are storage waste only, not a correctness problem.
	js, err := s.JSFor(ctx, key.AccountID)
	if err == nil {
		for _, p := range userPipes {
			_ = deletePipeStream(ctx, js, key.AccountID, p.Manifold, handle, p.Name)
		}
		for _, p := range src.Pipes() {
			_ = deletePipeStream(ctx, js, key.AccountID, src.Manifold, handle, p)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleDestroyPipe: DELETE /api/v1/sources/{handle}/pipes/{name}
//
// Removes the row + the JetStream stream. Returns 204 on success.
// Idempotent on missing stream (the row is the source of truth).
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
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	// Snapshot the row before it goes, so the audit trail can state what
	// the pipe retained. Missing row = an auto-pipe with no override;
	// the zero Pipe resolves to the defaults, which is the truth.
	prev, _ := db.GetPipeByName(ctx, s.Pool, src.ID, name)

	if err := db.DeletePipe(ctx, s.Pool, src.ID, name); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// Auto-provisioned pipes (broadcast, inbox, etc.) are
			// JetStream-only — not in the pipes table. Allow destroying
			// them directly via stream deletion rather than returning
			// E_PIPE_NOT_FOUND for pipes the user can see in ppz ls.
			if !src.IsAutoPipe(name) {
				writeErr(w, cliproto.NewPipeNotFound(name, handle))
				return
			}
			// fall through to JetStream cleanup below
		} else {
			writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
			return
		}
	}
	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "org account: " + err.Error()})
		return
	}
	if err := deletePipeStream(ctx, js, key.AccountID, src.Manifold, src.Handle, name); err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	// A destroy has no "after". `before` is the retention the pipe had —
	// resolved, so the row states what was actually lost rather than
	// which columns happened to be non-NULL.
	prevAge, prevMsgs, prevBytes := pipeRetention(prev)
	s.auditPipe(ctx, key, db.AuditActionPipeDestroy,
		cliproto.FormatPipePath(src.Manifold, src.Handle, name),
		snapshotRetention(prevAge, prevMsgs, prevBytes).mustJSON(), nil)

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
