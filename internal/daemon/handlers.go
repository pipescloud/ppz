package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/clock"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// natsObserveOptions returns the connection-state observation handlers.
// Both connect helpers below splat these onto every nats.Connect so
// disconnect / reconnect / closed transitions are captured by the
// daemon — surfaced by `ppz status` and `ppz diagnostics`.
//
// store receives one NATSEvent per nats.go callback firing; the daemon
// wires this to d.recordNATSEvent (ring + jsonl). nil disables capture
// (tests). jwtExpFn (optional) returns the unix-seconds `exp` of the
// JWT in use at event time, stamped onto every event so readers can
// spot "disconnect at jwt_exp == now" (the post-rotation-auth-violation
// pattern). nil for the static-cred helper (connectNATSWithJWT).
//
// Every event from this path carries Caller="nats.go" to distinguish
// library-initiated transitions from daemon-initiated ones (which carry
// the originating function name — see swapNC's "swap" events).
func natsObserveOptions(store func(NATSEvent), jwtExpFn func() int64) []nats.Option {
	record := func(typ, reason string, nc *nats.Conn) {
		if store == nil {
			return
		}
		var jwtExp int64
		if jwtExpFn != nil {
			jwtExp = jwtExpFn()
		}
		store(NATSEvent{
			Type:   typ,
			At:     time.Now(),
			Caller: "nats.go",
			NCID:   ncID(nc),
			JWTExp: jwtExp,
			Reason: reason,
		})
	}
	return []nats.Option{
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			reason := ""
			if err != nil {
				reason = err.Error()
			}
			record("disconnect", reason, nc)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			reason := ""
			if nc != nil {
				if u := nc.ConnectedUrl(); u != "" {
					reason = u
				}
			}
			record("reconnect", reason, nc)
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			reason := ""
			if nc != nil {
				if e := nc.LastError(); e != nil {
					reason = e.Error()
				}
			}
			record("closed", reason, nc)
		}),
	}
}

// connectNATSWithJWT connects to NATS at url, authenticating with the
// supplied User JWT + seed (Phase 3). Static — kept for legacy code
// paths that don't yet flow through the refresh loop. store may be nil
// to disable event capture.
func connectNATSWithJWT(url, userJWT, userSeed string, store func(NATSEvent)) (*nats.Conn, error) {
	if userJWT == "" || userSeed == "" {
		return nil, errors.New("connectNATSWithJWT: missing nats user jwt/seed in credentials")
	}
	kp, err := nkeys.FromSeed([]byte(userSeed))
	if err != nil {
		return nil, fmt.Errorf("parse user seed: %w", err)
	}
	opts := append([]nats.Option{
		nats.UserJWT(
			func() (string, error) { return userJWT, nil },
			func(nonce []byte) ([]byte, error) { return kp.Sign(nonce) },
		),
	}, natsObserveOptions(store, nil)...)
	return nats.Connect(url, opts...)
}

// connectNATSWithRefresh connects to NATS at url, reading the live
// User JWT + seed from the supplied RefreshLoop on every (re)connect.
// nats.go calls the callbacks once per connection establishment; if
// the refresh loop has rotated credentials in the meantime, the next
// reconnect picks up the fresh values. store may be nil to disable
// event capture; when set, every observe event is stamped with the
// current JWT exp from r so the post-rotation-auth-violation pattern
// can correlate disconnects with rotation timing.
func connectNATSWithRefresh(url string, r *RefreshLoop, store func(NATSEvent)) (*nats.Conn, error) {
	jwt, seed := r.Current()
	if jwt == "" || seed == "" {
		return nil, errors.New("connectNATSWithRefresh: refresh loop has no credentials yet")
	}
	opts := append([]nats.Option{
		nats.UserJWT(
			func() (string, error) {
				j, _ := r.Current()
				if j == "" {
					return "", errors.New("no jwt")
				}
				return j, nil
			},
			func(nonce []byte) ([]byte, error) {
				_, s := r.Current()
				if s == "" {
					return nil, errors.New("no seed")
				}
				kp, err := nkeys.FromSeed([]byte(s))
				if err != nil {
					return nil, err
				}
				return kp.Sign(nonce)
			},
		),
	}, natsObserveOptions(store, r.JWTExp)...)
	return nats.Connect(url, opts...)
}

// authExchangeRefresh is the RefreshFn we register with RefreshLoop.
// On every fire it re-runs POST /api/v1/auth/exchange with the
// daemon's bearer for the supplied accountID, and returns the new
// (jwt, seed, exp). 401 → ErrUnauthorized so the loop stops + the
// daemon's loginCheck flips to invalid.
func (d *Daemon) authExchangeRefresh(ctx context.Context, accountID string) (string, string, int64, error) {
	creds, ok := d.State.Credentials()
	if !ok {
		return "", "", 0, ErrUnauthorized
	}
	// Phase 4: thread the persisted AccountID so the refresh stays bound to
	// the org the user is currently switched to. Empty AccountID falls back
	// to the server's "first owned org" default — preserves behaviour
	// for daemons that haven't yet switched.
	body, _ := json.Marshal(cliproto.AuthExchangeRequest{APIKey: creds.APIKey, AccountID: d.State.AccountID()})
	req, err := http.NewRequestWithContext(ctx, "POST", creds.URL+"/api/v1/auth/exchange", bytes.NewReader(body))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", "", 0, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, fmt.Errorf("auth/exchange: HTTP %d", resp.StatusCode)
	}
	var ex cliproto.AuthExchangeReply
	if err := json.NewDecoder(resp.Body).Decode(&ex); err != nil {
		return "", "", 0, err
	}
	// Persist the refreshed creds so a daemon restart picks them up
	// without an immediate /auth/exchange round-trip.
	creds.NATSUserJWT = ex.NATSUserJWT
	creds.NATSUserSeed = ex.NATSUserSeed
	_ = d.State.SetLogin(*creds, ex.AccountID, ex.AccountName, keyPrefix(creds.APIKey))
	return ex.NATSUserJWT, ex.NATSUserSeed, ex.ExpiresAt.Unix(), nil
}

// startRefreshLoop swaps d.Refresh for a fresh RefreshLoop tied to
// the supplied initial credentials. Idempotent — stops any existing
// loop first.
func (d *Daemon) startRefreshLoop(accountID, jwt, seed string, expUnix int64) {
	if d.Refresh != nil {
		d.Refresh.Stop()
	}
	// refresh_error coalescing state for OnError below — see the hook's
	// comment. Closure-scoped so each loop generation starts clean.
	var (
		refreshErrMu         sync.Mutex
		lastRefreshErrReason string
		lastRefreshErrAt     time.Time
	)
	d.Refresh = &RefreshLoop{
		AccountID:   accountID,
		Refresh: d.authExchangeRefresh,
		OnUnauthorized: func(string) {
			d.State.SetLoginCheck(cliproto.LoginCheckInvalid)
		},
		// Record every failed refresh attempt WITH its cause. The
		// 2026-06-11 wake-from-sleep bundle had a ~70s window where
		// every /auth/exchange failed and the bundle couldn't say why —
		// ensureNATS collapses all transport errors to
		// E_SERVER_UNREACHABLE. This event is the durable trace
		// (`ppz diagnostics`, future bundles). Fires with no
		// RefreshLoop lock held, so reading d.Refresh.JWTExp() here is
		// deadlock-free.
		//
		// Coalesced: during a sustained outage the recovery loops
		// (refresh retry 5s, kickReconnect 2-15s, onWake 5s) fail with
		// an identical reason every few seconds; recording each one
		// would flood the bounded ring and evict the events that
		// diagnose the outage's START. An unchanged reason records at
		// most once per refreshErrorCoalesceWindow; a CHANGED reason
		// records immediately (DNS→TLS is signal). State is per-loop
		// closure: a re-login resets it along with the loop itself.
		OnError: func(err error) {
			reason := err.Error()
			refreshErrMu.Lock()
			dup := reason == lastRefreshErrReason &&
				time.Since(lastRefreshErrAt) < refreshErrorCoalesceWindow
			if !dup {
				lastRefreshErrReason, lastRefreshErrAt = reason, time.Now()
			}
			refreshErrMu.Unlock()
			if dup {
				return
			}
			d.recordNATSEvent(NATSEvent{
				Type:   "refresh_error",
				At:     time.Now(),
				Caller: "refresh-loop",
				JWTExp: d.Refresh.JWTExp(),
				Reason: reason,
			})
		},
		// Proactively rebuild the NATS connection with the fresh creds
		// during the 60s rotation overlap window (server `nbf` is set
		// 30s before issuance, refresh fires at exp-30s, so both JWTs
		// are valid for ~60s around the rotation point). This pre-empts
		// the server kicking the live connection at the old JWT's exp,
		// eliminating the ~3s disconnect/reconnect blip and the
		// transient E_NATS_UNREACHABLE that lands on any send running
		// inside it.
		//
		// connectNATSWithRefresh's callbacks read d.Refresh.Current()
		// lazily on (re)connect, and the refresh loop has just stamped
		// the fresh creds before invoking OnRefreshed — so the new
		// connection authenticates with the new JWT. Failures here are
		// best-effort: leaving the old NC in place falls back to the
		// pre-fix behaviour (server kicks at exp; nats.go reconnects),
		// so a transient connect failure during rotation is no worse
		// than today.
		OnRefreshed: func(_, _ string, _ int64) {
			// Proactive post-rotation reconnect, serialized + coalesced
			// with on-demand ensureNATS callers via rebuildNC/ncMu. The
			// generation check means whichever path runs first redials and
			// the other no-ops — no duelling swaps.
			_ = d.rebuildNC("OnRefreshed-callback")
		},
	}
	// context.Background — refresh loop outlives any single IPC
	// request. Stop() (called on Login replacement / Logout) is the
	// only way it ends.
	_ = d.Refresh.Start(context.Background(), jwt, seed, expUnix)
}

// handleLogin: POSTs the API key to /api/v1/auth/exchange, stores credentials
// + org ID, and (best-effort) opens a NATS connection.
func (d *Daemon) handleLogin(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.LoginRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	// Forward the org chosen in the device flow (empty for api-key logins,
	// where the key pins the org). The server mints the NATS JWT in this
	// org after validating the user's membership.
	body, _ := json.Marshal(cliproto.AuthExchangeRequest{APIKey: req.APIKey, AccountID: req.AccountID})
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", req.URL+"/api/v1/auth/exchange", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := d.HTTP.Do(httpReq)
	if err != nil {
		writeIPCErr(conn, cliproto.New(cliproto.EServerUnreachable))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		writeIPCErr(conn, cliproto.New(cliproto.EInvalidAPIKey))
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeIPCErr(conn, &cliproto.Error{Code: "E_HTTP", Message: resp.Status})
		return
	}
	var ex cliproto.AuthExchangeReply
	if err := json.NewDecoder(resp.Body).Decode(&ex); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}

	creds := Credentials{
		URL:          req.URL,
		APIKey:       req.APIKey,
		NATSUserJWT:  ex.NATSUserJWT,
		NATSUserSeed: ex.NATSUserSeed,
	}
	prefix := keyPrefix(req.APIKey)
	if err := d.State.SetLogin(creds, ex.AccountID, ex.AccountName, prefix); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	// PPZ_NATS_URL on the daemon overrides what the server told us. Used
	// when running the daemon outside compose: the server hands out
	// "nats://ppz-server:4222" (correct for in-compose clients) but a host
	// client needs "nats://localhost:4222" instead.
	natsURL := ex.NATSURL
	if v := os.Getenv("PPZ_NATS_URL"); v != "" {
		natsURL = v
	}
	d.NATSURL = natsURL

	// Connect (or reconnect) to NATS so broadcast/list paths can use
	// it. Phase 3.5: connection reads its JWT/seed from the refresh
	// loop on every (re)connect, so when the loop rotates creds the
	// next NATS reconnect picks them up automatically.
	//
	// swapNC handles the close-old/install-new dance AND evicts any
	// active follow conns — re-running login while a `terminal share`
	// is active would otherwise silently break the .stdin /.inbox
	// relays anchored to the prior NC.
	d.startRefreshLoop(ex.AccountID, ex.NATSUserJWT, ex.NATSUserSeed, ex.ExpiresAt.Unix())
	newNC, _ := connectNATSWithRefresh(natsURL, d.Refresh, d.recordNATSEvent)
	d.swapNC("handleLogin", newNC)
	if newNC != nil {
		if aid, err := uuid.Parse(ex.AccountID); err == nil {
			d.subscribeOrgHeartbeats(aid)
		}
	}

	writeIPC(conn, cliproto.LoginReply{URL: req.URL, KeyPrefix: prefix, AccountID: ex.AccountID})
}

// ensureNATS establishes the daemon's NATS connection from stored
// credentials. Login does this proactively, but a daemon restarted via
// `ppz kill` + `ppz daemon` only reloads creds from disk — d.NATSURL is
// in-memory state that doesn't survive. We rebuild it here on demand by
// re-calling /auth/exchange.
func (d *Daemon) ensureNATS(ctx context.Context) error {
	creds, ok := d.State.Credentials()
	if !ok {
		return cliproto.New(cliproto.ENotLoggedIn)
	}
	if err := d.bootstrapNATS(ctx, creds); err != nil {
		return err
	}
	// Single serialized rebuild: connects (or no-ops if already current on
	// this JWT generation), records the reconnect signal, and subscribes
	// org heartbeats. Coalesces with the OnRefreshed goroutine via ncMu so
	// a rotation triggers exactly one swap — no burst-swap-storm. Runs
	// OUTSIDE bootstrapMu (it has its own ncMu); holding both across the
	// dial is unnecessary and would widen the lock.
	return d.rebuildNC("ensureNATS")
}

// bootstrapNATS single-flights (via bootstrapMu) the credential + URL
// preparation that must happen before rebuildNC can dial: refreshing the
// JWT if due, and the one-time cold /auth/exchange that populates
// d.NATSURL and the refresh loop on a freshly restarted daemon. The
// NATSURL=="" check doubles as the double-check — the second caller to
// acquire the lock finds it set and skips the exchange. See bootstrapMu.
func (d *Daemon) bootstrapNATS(ctx context.Context, creds *Credentials) error {
	d.bootstrapMu.Lock()
	defer d.bootstrapMu.Unlock()
	d.ensureRefreshLoopFromCreds(creds)
	if d.Refresh != nil {
		// Refresh creds if the JWT is within its rotation window. We do NOT
		// drop the NC here on rotation: rebuildNC's generation check
		// (ncExp vs Refresh.JWTExp) redials exactly once afterward and
		// coalesces concurrent ensureNATS callers + the OnRefreshed
		// goroutine — the burst-swap-storm fix. (Previously each rotation
		// did swapNC(nil) here, racing N reconnects.)
		if _, err := d.Refresh.RefreshNowIfDue(ctx, time.Now()); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				return cliproto.New(cliproto.EInvalidAPIKey)
			}
			return cliproto.New(cliproto.EServerUnreachable)
		}
	}
	if d.NATSURL == "" {
		// Carry the org the user logged into. Without it the server's OAuth
		// branch falls back to its default org and mints the NATS JWT there
		// — desyncing it from the AccountID we stamp on ?org= list calls.
		body, _ := json.Marshal(cliproto.AuthExchangeRequest{APIKey: creds.APIKey, AccountID: creds.AccountID})
		req, err := http.NewRequestWithContext(ctx, "POST", creds.URL+"/api/v1/auth/exchange", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := d.HTTP.Do(req)
		if err != nil {
			return cliproto.New(cliproto.EServerUnreachable)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			// Distinguish a genuine auth failure (terminal — retrying
			// can't fix a revoked/invalid key) from a transient server
			// error (5xx/429 — retryable). Collapsing both to
			// EInvalidAPIKey made the background connect loop
			// (kickConnect) give up forever when the server was merely
			// down/overloaded during the restart window. 401/403 mirror
			// the refresh path's ErrUnauthorized treatment.
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return cliproto.New(cliproto.EInvalidAPIKey)
			}
			return cliproto.New(cliproto.EServerUnreachable)
		}
		var ex cliproto.AuthExchangeReply
		if err := json.NewDecoder(resp.Body).Decode(&ex); err != nil {
			return err
		}
		natsURL := ex.NATSURL
		if v := os.Getenv("PPZ_NATS_URL"); v != "" {
			natsURL = v
		}
		d.NATSURL = natsURL
		// Refresh persisted creds with the new JWT/seed so reconnects
		// don't depend on stale on-disk values.
		creds.NATSUserJWT = ex.NATSUserJWT
		creds.NATSUserSeed = ex.NATSUserSeed
		// Adopt the account the server actually minted the JWT in — not the
		// stale persisted AccountID — so State.AccountID() (used for ?org=
		// listing and every NATS subject) stays bound to the live creds.
		creds.AccountID = ex.AccountID
		creds.AccountName = ex.AccountName
		_ = d.State.SetLogin(*creds, ex.AccountID, ex.AccountName, keyPrefix(creds.APIKey))
		d.startRefreshLoop(ex.AccountID, ex.NATSUserJWT, ex.NATSUserSeed, ex.ExpiresAt.Unix())
	}
	// If we got here without /auth/exchange (NATSURL was already known) but
	// the refresh loop never started (e.g. fresh daemon process
	// post-restart), boot it from the persisted creds.
	d.ensureRefreshLoopFromCreds(creds)
	return nil
}

func (d *Daemon) ensureRefreshLoopFromCreds(creds *Credentials) {
	if d.Refresh != nil || creds.NATSUserJWT == "" || creds.NATSUserSeed == "" {
		return
	}
	d.startRefreshLoop(creds.AccountID, creds.NATSUserJWT, creds.NATSUserSeed, time.Now().Add(-time.Minute).Unix())
}

// authHTTP sets the bearer header on a request.
func (d *Daemon) authHTTP(req *http.Request) error {
	creds, ok := d.State.Credentials()
	if !ok {
		return cliproto.New(cliproto.ENotLoggedIn)
	}
	req.Header.Set("Authorization", "Bearer "+creds.APIKey)
	return nil
}

// callServer is a thin JSON-in / JSON-out helper for daemon → server calls.
//
// Phase 4: when the daemon has a stored AccountID (set by login or `org
// switch`), we stamp it as `?org=<id>` on every call. The server's
// requireAPIKey middleware uses it to scope OAuth-bearer requests to
// the right tenant; without this, /api/v1/sources etc. would silently
// fall back to FirstOwnedOrgFor and the source would land in the
// wrong org. API-key callers also get the param, which the server
// validates against the key's org (a mismatch is an explicit error,
// preferred to silent ambiguity).
func (d *Daemon) callServer(ctx context.Context, method, path string, body any, out any) *cliproto.Error {
	creds, ok := d.State.Credentials()
	if !ok {
		return cliproto.New(cliproto.ENotLoggedIn)
	}
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}
	url := creds.URL + path
	if accountID := d.State.AccountID(); accountID != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		url += sep + "org=" + accountID
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+creds.APIKey)
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		// Network failure isn't an auth verdict — leave the cached
		// loginCheck alone. Status will surface the cached state (or
		// probe if unobserved).
		return cliproto.New(cliproto.EServerUnreachable)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var herr cliproto.HTTPError
		if err := json.NewDecoder(resp.Body).Decode(&herr); err == nil && herr.Error.Code != "" {
			e := herr.Error
			if e.Code == cliproto.EInvalidAPIKey {
				d.State.SetLoginCheck(cliproto.LoginCheckInvalid)
			}
			return &e
		}
		return &cliproto.Error{Code: "E_HTTP", Message: resp.Status}
	}
	// 2xx: the credential proved itself, so flip the cache to ok. This
	// is the path that resets a stale "invalid" observation once the
	// user has run `ppz login` to refresh.
	d.State.SetLoginCheck(cliproto.LoginCheckOK)
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()}
	}
	return nil
}

func (d *Daemon) handleCreate(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.CreateRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	if err := natsubj.ValidateHandle(req.Handle); err != nil {
		writeIPCErr(conn, cliproto.NewInvalidHandle(req.Handle))
		return
	}
	// Phase 1.5.1: source create is namespace-aware. If the request
	// didn't pin a manifold explicitly (CLI doesn't), pull from the
	// session's current namespace. Sources at non-root manifold are
	// the basis for multi-team self-hosters + pipescloud's hierarchy.
	manifold := req.Manifold
	if manifold == "" {
		manifold = d.State.CurrentNamespace(req.Session)
	}
	var reply cliproto.CreateSourceReply
	if e := d.callServer(ctx, "POST", "/api/v1/sources",
		cliproto.CreateSourceRequest{Handle: req.Handle, Kind: req.Kind, Manifold: manifold}, &reply); e != nil {
		writeIPCErr(conn, e)
		return
	}
	d.State.RememberSource(reply.Handle, reply.Manifold)
	// Auto-subscribe the new handle to its own inbox, keyed under the
	// HANDLE — visible to every subprocess inside `ppz terminal share
	// <handle>` / `ppz agent create <handle>` (PPZ_SESSION=<handle>).
	// Idempotent; one hook covers source create, terminal share, and agent
	// create — all route here.
	_ = d.Subs.Add(reply.Handle, reply.Handle+".inbox")
	// PTY sources don't become the daemon's "current" — the user retains
	// their existing current message source so `ppz send` keeps working
	// the way they expect outside the terminal.
	if req.Kind != string(cliproto.KindPTY) {
		if err := d.State.SetCurrent(req.Session, reply.Handle); err != nil {
			writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
			return
		}
		// `source create` makes the CREATING session operate as the handle
		// (current set above), so surface its inbox in that session's subs
		// too — a plain `ppz subs ls/wait` from the shell that ran
		// `source create` then just works. The pty paths above deliberately
		// skip this: the operator stays themselves, so the agent's inbox
		// must not leak into the operator's personal subs list.
		_ = d.Subs.Add(req.Session, reply.Handle+".inbox")
	}
	writeIPC(conn, cliproto.CreateReply{Handle: reply.Handle, Manifold: reply.Manifold, Subject: reply.Subject})
}

// handleConnect is the daemon-side combo verb for `ppz connect <handle>`:
// idempotently ensure the source exists, then SetCurrent. Treats
// E_SOURCE_TAKEN from the server as "already exists, nothing to do" and
// proceeds to switch.
//
// Today (Phase C) the source-create path also auto-provisions the broadcast
// JetStream stream (carried over from the pre-refactor behaviour). When
// user-creatable pipes land (Phase E), the stream-creation step here may
// move into a dedicated "ensure broadcast pipe" step so source create can
// stop provisioning streams of its own.
func (d *Daemon) handleConnect(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.ConnectRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	if _, ok := d.State.Credentials(); !ok {
		writeIPCErr(conn, cliproto.New(cliproto.ENotLoggedIn))
		return
	}
	if err := natsubj.ValidateHandle(req.Handle); err != nil {
		writeIPCErr(conn, cliproto.New(cliproto.EInvalidHandle))
		return
	}

	var reply cliproto.CreateSourceReply
	if e := d.callServer(ctx, "POST", "/api/v1/sources",
		cliproto.CreateSourceRequest{Handle: req.Handle}, &reply); e != nil {
		// Idempotent: source already exists is success.
		if e.Code != cliproto.ESourceTaken {
			writeIPCErr(conn, e)
			return
		}
	}
	d.State.RememberPipe(req.Handle)
	if err := d.State.SetCurrent(req.Session, req.Handle); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	// Same invariant as handleSwitch/handleCreate: adopting the handle as
	// the session's current subscribes its inbox. Idempotent.
	_ = d.Subs.Add(req.Session, req.Handle+".inbox")
	writeIPC(conn, cliproto.ConnectReply{Handle: req.Handle})
}

// handlePipeCreate proxies `ppz pipe create` to the server. Daemon-side
// validation; server validates again and provisions the JetStream stream.
//
// Phase 1.5 routing: when req.Handle is empty AND req.SourceHandle is nil
// (or empty), this is the uncollared shape — route to the sourceless
// endpoint POST /api/v1/pipes. Otherwise it's collared — route to the
// existing POST /api/v1/sources/{handle}/pipes shortcut.
func (d *Daemon) handlePipeCreate(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.PipeCreateRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	if _, ok := d.State.Credentials(); !ok {
		writeIPCErr(conn, cliproto.New(cliproto.ENotLoggedIn))
		return
	}
	if err := natsubj.ValidateUserPipeName(req.Name); err != nil {
		if err.Error() == "name is reserved" {
			writeIPCErr(conn, cliproto.NewInvalidPipeReserved(req.Name))
		} else {
			writeIPCErr(conn, cliproto.NewInvalidPipeName(req.Name))
		}
		return
	}

	// Decide collared vs uncollared from the request shape.
	collared := req.Handle != "" || (req.SourceHandle != nil && *req.SourceHandle != "")

	// Phase 1.5: stamp Manifold from the daemon's per-session namespace if
	// the request didn't carry one explicitly. CLI users set namespace via
	// `ppz set namespace`; the daemon-side stamping keeps that
	// transparent. Explicit Manifold on the request (e.g. for callers that
	// know what they want) takes precedence.
	if req.Manifold == "" {
		req.Manifold = d.State.CurrentNamespace(req.Session)
	}
	// Don't leak the session field to the server — it's daemon-side only.
	req.Session = ""

	var reply cliproto.PipeCreateReply
	if collared {
		// Resolve the handle from either field, prefer SourceHandle (new
		// Phase 1.5 field) over Handle (legacy field).
		handle := req.Handle
		if req.SourceHandle != nil && *req.SourceHandle != "" {
			handle = *req.SourceHandle
		}
		if err := natsubj.ValidateHandle(handle); err != nil {
			writeIPCErr(conn, cliproto.New(cliproto.EInvalidHandle))
			return
		}
		// Normalise so the server sees the handle in the request body's
		// legacy Handle field (the collared endpoint reads from there).
		req.Handle = handle
		if e := d.callServer(ctx, "POST", "/api/v1/sources/"+handle+"/pipes", req, &reply); e != nil {
			writeIPCErr(conn, e)
			return
		}
	} else {
		// Uncollared — Phase 1.5 sourceless pipe.
		if e := d.callServer(ctx, "POST", "/api/v1/pipes", req, &reply); e != nil {
			writeIPCErr(conn, e)
			return
		}
	}
	writeIPC(conn, reply)
}

// handleSourceDestroy proxies `ppz source destroy HANDLE` to the server.
// On success it clears every session whose current equals the destroyed
// handle and removes it from the known-pipes cache.
func (d *Daemon) handleSourceDestroy(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.SourceDestroyRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	if _, ok := d.State.Credentials(); !ok {
		writeIPCErr(conn, cliproto.New(cliproto.ENotLoggedIn))
		return
	}
	if err := natsubj.ValidateHandle(req.Handle); err != nil {
		writeIPCErr(conn, cliproto.NewInvalidHandle(req.Handle))
		return
	}
	// Look up the source's manifold via the handle→manifold cache BEFORE
	// the delete clears it from any subsequent ls refresh. Empty if the
	// source is at root (or cache miss — fallback to root display).
	manifold := d.State.HandleManifold(req.Handle)
	if e := d.callServer(ctx, "DELETE", "/api/v1/sources/"+req.Handle, nil, nil); e != nil {
		writeIPCErr(conn, e)
		return
	}
	d.State.ForgetPipe(req.Handle)
	_ = d.State.ClearCurrentForHandle(req.Handle)
	// Cascade: drop any subscription targeting the destroyed handle's pipes
	// from every session's subs file — no zombie subs across recreate.
	// Mirrors the cursor sweep pattern.
	_ = d.Subs.SweepHandle(req.Handle)
	writeIPC(conn, cliproto.SourceDestroyReply{Handle: req.Handle, Manifold: manifold})
}

// handlePipeDestroy proxies `ppz pipe destroy` to the server.
//
// Phase 1.5 routing: BareTarget set + no Handle → uncollared destroy
// via DELETE /api/v1/pipes; otherwise collared destroy via the legacy
// per-source path.
func (d *Daemon) handlePipeDestroy(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.PipeDestroyRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	if _, ok := d.State.Credentials(); !ok {
		writeIPCErr(conn, cliproto.New(cliproto.ENotLoggedIn))
		return
	}

	if req.Handle == "" && req.BareTarget != "" {
		// Uncollared destroy.
		if err := natsubj.ValidatePipe(req.BareTarget); err != nil {
			writeIPCErr(conn, cliproto.New(cliproto.EInvalidPipe))
			return
		}
		// Explicit Manifold wins over session lookup — callers that
		// already know the target pipe's manifold (the glob path) need
		// to address pipes across namespaces, not just the session's.
		manifold := req.Manifold
		if manifold == "" {
			manifold = d.State.CurrentNamespace(req.Session)
		}
		q := url.Values{}
		q.Set("name", req.BareTarget)
		if manifold != "" {
			q.Set("manifold", manifold)
		}
		if e := d.callServer(ctx, "DELETE", "/api/v1/pipes?"+q.Encode(), nil, nil); e != nil {
			writeIPCErr(conn, e)
			return
		}
		writeIPC(conn, cliproto.PipeDestroyReply{Manifold: manifold, Name: req.BareTarget})
		return
	}

	if err := natsubj.ValidateHandle(req.Handle); err != nil {
		writeIPCErr(conn, cliproto.New(cliproto.EInvalidHandle))
		return
	}
	if e := d.callServer(ctx, "DELETE", "/api/v1/sources/"+req.Handle+"/pipes/"+req.Name, nil, nil); e != nil {
		writeIPCErr(conn, e)
		return
	}
	writeIPC(conn, cliproto.PipeDestroyReply{Handle: req.Handle, Name: req.Name})
}

// handleDisconnect clears the calling session's current source. Source
// itself stays provisioned. Other sessions' currents are unaffected.
// Idempotent — calling on no-current is fine.
func (d *Daemon) handleDisconnect(_ context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.DisconnectRequest
	_ = json.Unmarshal(params, &req) // optional body — empty is fine
	if _, ok := d.State.Credentials(); !ok {
		writeIPCErr(conn, cliproto.New(cliproto.ENotLoggedIn))
		return
	}
	if err := d.State.ClearCurrent(req.Session); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	writeIPC(conn, cliproto.DisconnectReply{})
}

func (d *Daemon) handleSwitch(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.SwitchRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	if _, ok := d.State.Credentials(); !ok {
		writeIPCErr(conn, cliproto.New(cliproto.ENotLoggedIn))
		return
	}
	// Verify the handle exists server-side. Refresh the cache from /api/v1/sources.
	var lr cliproto.ListSourcesReply
	if e := d.callServer(ctx, "GET", "/api/v1/sources", nil, &lr); e != nil {
		writeIPCErr(conn, e)
		return
	}
	d.refreshSourceCache(lr.Sources)
	if !d.State.KnowsPipe(req.Handle) {
		writeIPCErr(conn, cliproto.NewSourceNotFound(req.Handle))
		return
	}
	if err := d.State.SetCurrent(req.Session, req.Handle); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	// Adopting H as this session's current means the session now operates
	// as H — surface H's inbox in its subs so a plain `ppz subs ls/wait`
	// works after `ppz set handle` (mirrors source create; see
	// handleCreate). Idempotent.
	_ = d.Subs.Add(req.Session, req.Handle+".inbox")
	writeIPC(conn, cliproto.SwitchReply{Handle: req.Handle})
}

// handleSetNamespace stores the per-session manifold. Phase 1.5 (locked
// decision #20 — `ppz set namespace PATH`). Validates each dot-separated
// segment via the existing handle regex; empty namespace is a no-op clear.
func (d *Daemon) handleSetNamespace(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.SetNamespaceRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	if _, ok := d.State.Credentials(); !ok {
		writeIPCErr(conn, cliproto.New(cliproto.ENotLoggedIn))
		return
	}
	// Empty namespace via `set` is treated as clear — same behaviour as
	// `unset namespace`, so users can `set namespace ""` interchangeably.
	if req.Namespace != "" {
		for _, seg := range strings.Split(req.Namespace, ".") {
			if err := natsubj.ValidateHandle(seg); err != nil {
				writeIPCErr(conn, &cliproto.Error{Code: cliproto.EInvalidManifold, Message: "namespace segment invalid: " + seg})
				return
			}
		}
	}
	if err := d.State.SetNamespace(req.Session, req.Namespace); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	writeIPC(conn, cliproto.SetNamespaceReply{Namespace: req.Namespace})
}

// handleUnsetNamespace clears the per-session manifold. Idempotent.
func (d *Daemon) handleUnsetNamespace(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.UnsetNamespaceRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	if _, ok := d.State.Credentials(); !ok {
		writeIPCErr(conn, cliproto.New(cliproto.ENotLoggedIn))
		return
	}
	if err := d.State.ClearNamespace(req.Session); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	writeIPC(conn, cliproto.UnsetNamespaceReply{})
}

func (d *Daemon) handleSend(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.SendRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	// Trust-boundary check (v0.25.0, spec §3): the `ack:` subject prefix is
	// reserved for daemon-emitted protocol messages. CLI argument
	// validation is belt; this is suspenders — any IPC client (custom
	// scripts, third-party tools, harness adapters) hits this same path.
	// Daemon-internal ack auto-emission (§4) bypasses handleSend and
	// publishes envelopes directly, so this rule has no exception.
	if strings.HasPrefix(req.MsgSubject, "ack:") {
		writeIPCErr(conn, cliproto.New(cliproto.EInvalidSubject))
		return
	}
	target, e := d.resolveSendTarget(ctx, req.Handle, req.Channel, req.BareTarget, req.Session, req.Sender)
	if e != nil {
		writeIPCErr(conn, e)
		return
	}
	env := buildBroadcastEnvelope(req, target.sender, clock.Now())
	data, err := env.Marshal()
	if err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	if len(data) > envelope.MaxBytes {
		writeIPCErr(conn, cliproto.New(cliproto.EPayloadTooLarge))
		return
	}
	// Publish and BLOCK for the JetStream PubAck. The reply below is
	// written only after a confirmed durable write — so `sent id=…`
	// exit 0 means the message is genuinely on the server (contract
	// clause 1). A core Publish+Flush would return "ok" for a message
	// dropped across a reconnect window (Bug B silent loss).
	if e := d.publishWithAck(target.subject, data); e != nil {
		writeIPCErr(conn, e)
		return
	}
	// Heartbeat fast-path: stamp the daemon's in-memory cache so
	// `ppz who` can render this agent without a NATS round-trip.
	// No-op on every other channel.
	applyHeartbeatStamp(d.Heartbeats, req.Channel, req.Handle, req.Payload, clock.Now())
	// Bytes counts the user-visible payload, not the encoded envelope —
	// matches WIRE.md §8 ppz broadcast and the broadcast-from-* fixtures.
	writeIPC(conn, cliproto.SendReply{ID: env.ID, Subject: target.subject, Bytes: len(req.Payload)})
}

// handleSendBatch publishes N payloads in one IPC round-trip.
// Validation runs once for the whole batch; the daemon then issues N
// async nc.Publish calls followed by a SINGLE nc.Flush. Same "bytes
// confirmed at server" contract as handleSend — just amortised
// across the batch. Used by streaming producers (terminal share's
// stdout drain, `ppz broadcast` line-streaming) where the per-call
// flush cost dominates throughput under WAN latency.
func (d *Daemon) handleSendBatch(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.SendBatchRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	if len(req.Payloads) == 0 {
		writeIPC(conn, cliproto.SendBatchReply{})
		return
	}
	// Batch path passes "" for reqSender: the only caller is the share
	// parent's stdout/stdctrl publisher, where the CLI-side env doesn't
	// represent the wrapped handle. See SendBatchRequest doc comment.
	target, e := d.resolveSendTarget(ctx, req.Handle, req.Channel, req.BareTarget, req.Session, "")
	if e != nil {
		writeIPCErr(conn, e)
		return
	}
	// Pre-validate every payload (size + marshal) BEFORE any publish so
	// an oversize entry rejects the whole batch deterministically rather
	// than landing the prefix on the server and then erroring.
	ids := make([]string, 0, len(req.Payloads))
	bytes := make([]int, 0, len(req.Payloads))
	datas := make([][]byte, 0, len(req.Payloads))
	now := clock.Now()
	for _, payload := range req.Payloads {
		env := envelope.New(target.sender, "", payload, now)
		data, err := env.Marshal()
		if err != nil {
			writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
			return
		}
		if len(data) > envelope.MaxBytes {
			writeIPCErr(conn, cliproto.New(cliproto.EPayloadTooLarge))
			return
		}
		datas = append(datas, data)
		ids = append(ids, env.ID)
		bytes = append(bytes, len(payload))
	}
	// One PublishAsync per message + one batched ack wait covering all.
	// Contract clause 1: `sent` exit 0 ⟹ every message durably stored.
	if e := d.publishBatchWithAck(target.subject, datas); e != nil {
		writeIPCErr(conn, e)
		return
	}
	// Heartbeat fast-path on the batch path. In practice the heartbeat
	// ticker publishes one beat at a time via handleSend, so this is
	// belt-and-suspenders — if any caller ever batches heartbeats the
	// cache still gets the latest payload.
	if len(req.Payloads) > 0 {
		applyHeartbeatStamp(d.Heartbeats, req.Channel, req.Handle, req.Payloads[len(req.Payloads)-1], clock.Now())
	}
	writeIPC(conn, cliproto.SendBatchReply{IDs: ids, Subject: target.subject, Bytes: bytes})
}

// shouldTryUncollaredFirst reports whether the daemon should attempt
// uncollared resolution before falling back to the legacy collared
// `<bareTarget>.inbox` shorthand. Pure function so the decision is
// unit-testable without standing up NATS / the daemon.
//
// Rule (Phase 1.5.1): try uncollared first whenever the CLI signalled
// a bare target. If the uncollared stream exists, publish there. If
// not, fall back to the legacy interpretation (the CLI synthesises
// reqHandle=bareTarget + channel=inbox for backward-compat with the
// `ppz send foo` → `foo.inbox` messaging shorthand).
//
// The pre-1.5.1 rule additionally gated on "no current handle from
// any source", which was wrong because the CLI's own .inbox sugar
// always populates reqHandle for bare names — so the gate never
// passed and uncollared sends always 404'd as E_SOURCE_NOT_FOUND.
func shouldTryUncollaredFirst(bareTarget string) bool {
	return bareTarget != ""
}

// uncollaredCursorKey is the per-session cursor key shape for uncollared
// pipe reads. Phase 1.5: namespaced with the full subject so two
// uncollared pipes named the same in different manifolds don't share a
// cursor. Pinned by tests so handleRead (read.go) and uncollaredPipeInfo
// (handlers.go) stay in sync.
func uncollaredCursorKey(filterSubject string) string {
	return "uncollared:" + filterSubject
}

// sendTarget bundles the resolved facts a publish needs: the
// destination subject + the sender id we stamp into the envelope.
type sendTarget struct {
	subject string
	sender  string
	// Resolved subject parts, for callers that need them structured
	// (schedule create forwards them to the server instead of
	// publishing). source=="" means an uncollared pipe at manifold.
	manifold string
	source   string
	pipe     string
}

// resolveSendTarget runs the shared pre-flight for a broadcast:
// login check, target resolution (request handle, env, session
// current), pipe-name validation, server-side source existence (with
// stale-current cleanup), JetStream stream existence, and ensureNATS.
// Returns the destination subject + sender id on success. Used by
// both handleSend (single) and handleSendBatch (N).
// reqSender is the CLI's hint (PPZ_CURRENT_HANDLE forwarded from the
// calling shell). senderForRequest decides whether it wins over the
// daemon's per-session state — kept in one helper so the precedence
// rule has a single audit point.
func (d *Daemon) resolveSendTarget(ctx context.Context, reqHandle, reqChannel, bareTarget, session, reqSender string) (sendTarget, *cliproto.Error) {
	if _, ok := d.State.Credentials(); !ok {
		return sendTarget{}, cliproto.New(cliproto.ENotLoggedIn)
	}
	accountID, err := uuid.Parse(d.State.AccountID())
	if err != nil {
		return sendTarget{}, &cliproto.Error{Code: "E_INTERNAL", Message: "bad account id"}
	}
	// Phase 1.5.1: try uncollared resolution first if the CLI signalled
	// a bare target. If the uncollared stream exists in the session's
	// current namespace, publish there. If not, fall through to the
	// legacy collared interpretation (the CLI synthesised reqHandle =
	// bareTarget + channel = "inbox" for back-compat with the messaging
	// shorthand `ppz send foo` → `foo.inbox`).
	//
	// The first-wins collision rule at create time means uncollared X
	// and source X can never coexist at the same manifold, so the
	// runtime check is unambiguous: at most one shape is real, and the
	// fall-through finds it.
	//
	// Sender stamping is per the success-branch comment below.
	if shouldTryUncollaredFirst(bareTarget) {
		if err := natsubj.ValidatePipe(bareTarget); err != nil {
			return sendTarget{}, cliproto.New(cliproto.EInvalidPipe)
		}
		manifold := d.State.CurrentNamespace(session)
		if d.NC == nil || !d.NC.IsConnected() {
			if err := d.ensureNATS(ctx); err != nil {
				return sendTarget{}, cliproto.New(cliproto.ENATSUnreachable)
			}
		}
		js, jsErr := d.jetStream()
		if jsErr == nil {
			_, err := js.Stream(ctx, natsubj.BuildStreamName(accountID, manifold, "", bareTarget))
			switch {
			case err == nil:
				// Phase 1.5.3: stamp sender from the session's current
				// handle when one is set. Empty otherwise (anonymous
				// send). Mirrors how collared sends carry the source
				// handle as the actor identity.
				//
				// Sender selection routed through senderForRequest so
				// CLI-supplied hints (env-resolved current) override
				// the daemon's per-session state in a single place.
				return sendTarget{
					subject:  natsubj.BuildSubject(accountID, manifold, "", bareTarget),
					sender:   senderForRequest(reqSender, d.State.Current(session)),
					manifold: manifold,
					pipe:     bareTarget,
				}, nil
			case errors.Is(err, jetstream.ErrStreamNotFound):
				// Fall through to the legacy collared interpretation.
			case errors.Is(err, context.DeadlineExceeded),
				errors.Is(err, nats.ErrTimeout),
				errors.Is(err, nats.ErrConnectionClosed),
				errors.Is(err, nats.ErrNoServers):
				if !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrNoServers) {
					d.reportNATSFailure()
				}
				return sendTarget{}, cliproto.New(cliproto.ENATSUnreachable)
			default:
				return sendTarget{}, cliproto.New(cliproto.EInvalidPipe)
			}
		}
	}
	// Resolve target. Explicit handle from the request wins (used by
	// `ppz send` and by terminal-share when PPZ_CURRENT_HANDLE is
	// set); otherwise fall back to the calling session's current
	// handle — different terminal windows have different "current"s.
	fromCurrent := reqHandle == "" && os.Getenv("PPZ_CURRENT_HANDLE") == ""
	current := reqHandle
	if current == "" {
		current = d.State.Current(session)
	}
	if current == "" {
		return sendTarget{}, cliproto.New(cliproto.ENoCurrentSource)
	}
	pipe := reqChannel
	if pipe == "" {
		pipe = "broadcast"
	}
	if err := natsubj.ValidatePipe(pipe); err != nil {
		return sendTarget{}, cliproto.New(cliproto.EInvalidPipe)
	}
	// Verify the handle is a real source in this org. NATS publish
	// silently succeeds against any subject, so without this check a
	// stale handle (wrapped terminal whose source was deleted, or a
	// session-current pointing at a server-side-deleted source) would
	// return `sent id=...` while the message vanishes.
	//
	// For implicit (session-current) targets, ALWAYS refresh from the
	// server: the daemon's KnowsPipe cache is durable across daemon
	// lifetimes; without a refresh, a stale per-session current set in
	// a previous shell can produce a confusing "source not found"
	// error pointing at a handle the user doesn't remember setting.
	needRefresh := !d.State.KnowsPipe(current) || fromCurrent
	if needRefresh {
		var lr cliproto.ListSourcesReply
		if e := d.callServer(ctx, "GET", "/api/v1/sources", nil, &lr); e != nil {
			return sendTarget{}, e
		}
		d.refreshSourceCache(lr.Sources)
		if !d.State.KnowsPipe(current) {
			if fromCurrent {
				_ = d.State.ClearCurrent(session)
				return sendTarget{}, cliproto.New(cliproto.ENoCurrentSource)
			}
			return sendTarget{}, cliproto.NewSourceNotFound(current)
		}
	}
	if d.NC == nil || !d.NC.IsConnected() {
		if err := d.ensureNATS(ctx); err != nil {
			return sendTarget{}, cliproto.New(cliproto.ENATSUnreachable)
		}
	}
	// Verify the JetStream stream exists before publishing. NATS-core
	// publish to a subject with no consumer silently succeeds.
	//
	// Classify js.Stream() errors into the right user-facing code:
	//   - jetstream.ErrStreamNotFound → E_PIPE_NOT_FOUND: the pipe
	//     genuinely doesn't exist on this source. Names the pipe + source
	//     in the message so the user sees the actionable next step.
	//   - context.DeadlineExceeded / nats.ErrTimeout / nats.ErrConnectionClosed
	//     → E_NATS_UNREACHABLE: can't reach the broker to even check.
	//     The pipe might be perfectly valid (see the wifi-disconnect
	//     repro in MoltHub agent feedback); attributing the failure to
	//     pipe invalidity misled agents away from the real cause.
	//   - any other error → E_INVALID_PIPE catch-all. Truly unexpected.
	currentManifold := d.State.HandleManifold(current)
	if js, err := d.jetStream(); err == nil {
		if _, err := js.Stream(ctx, natsubj.BuildStreamName(accountID, currentManifold, current, pipe)); err != nil {
			switch {
			case errors.Is(err, jetstream.ErrStreamNotFound):
				return sendTarget{}, cliproto.NewPipeNotFound(pipe, current)
			case errors.Is(err, context.DeadlineExceeded),
				errors.Is(err, nats.ErrTimeout),
				errors.Is(err, nats.ErrConnectionClosed),
				errors.Is(err, nats.ErrNoServers):
				if !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrNoServers) {
					d.reportNATSFailure()
				}
				return sendTarget{}, cliproto.New(cliproto.ENATSUnreachable)
			default:
				return sendTarget{}, cliproto.New(cliproto.EInvalidPipe)
			}
		}
	}
	return sendTarget{
		subject:  natsubj.BuildSubject(accountID, currentManifold, current, pipe),
		sender:   senderForRequest(reqSender, d.State.Current(session)),
		manifold: currentManifold,
		source:   current,
		pipe:     pipe,
	}, nil
}

// refreshSourceCache replaces the daemon's known-handles set and
// handle→manifold cache from a freshly-fetched /api/v1/sources
// response. Both list paths (unfiltered handleList and pattern-
// filtered buildFilteredList) call this — downstream paths that
// read HandleManifold without self-healing (handleRead at
// read.go:106, ack-emit publishEnvelope at publish.go:130)
// otherwise misroute to the root manifold after a list as the
// first daemon interaction following a restart.
func (d *Daemon) refreshSourceCache(sources []cliproto.Source) {
	handles := make([]string, 0, len(sources))
	manifolds := make(map[string]string, len(sources))
	completion := make([]cliproto.CompleteSource, 0, len(sources))
	for _, s := range sources {
		handles = append(handles, s.Handle)
		manifolds[s.Handle] = s.Manifold
		// Build the completion view alongside the existing caches.
		// Auto-pipes (pipesForKind) PLUS user-created pipes (Source.Pipes
		// from /api/v1/sources). De-dupe in case the server ever ships
		// an auto-pipe name in the user list — defensive, costs O(k).
		seen := map[string]bool{}
		var pipes []string
		for _, p := range pipesForKind(s.Kind) {
			if seen[p] {
				continue
			}
			seen[p] = true
			pipes = append(pipes, p)
		}
		for _, p := range s.Pipes {
			if seen[p] {
				continue
			}
			seen[p] = true
			pipes = append(pipes, p)
		}
		completion = append(completion, cliproto.CompleteSource{
			Handle: s.Handle,
			Pipes:  pipes,
		})
	}
	d.State.ResetSources(handles, manifolds)
	d.State.SetCompletionSnapshot(completion)
}

func (d *Daemon) handleList(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.ListRequest
	_ = json.Unmarshal(params, &req) // optional body

	if _, ok := d.State.Credentials(); !ok {
		writeIPCErr(conn, cliproto.New(cliproto.ENotLoggedIn))
		return
	}

	// With patterns, delegate to the same filtering path --watch uses
	// so `ppz ls foo%` and `ppz ls --watch foo%` agree on what
	// matches. Without patterns, fall through to the original
	// unfiltered enumeration — every other IPCList caller (source.go,
	// pipe.go, completion.go) takes this path and expects
	// the full snapshot.
	if len(req.Patterns) > 0 {
		if err := d.ensureNATS(ctx); err != nil {
			if e, ok := err.(*cliproto.Error); ok {
				writeIPCErr(conn, e)
			} else {
				writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
			}
			return
		}
		accountID, err := uuid.Parse(d.State.AccountID())
		if err != nil {
			writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: "bad org id"})
			return
		}
		reply, e := d.buildFilteredList(ctx, accountID, req.Session, req.Patterns)
		if e != nil {
			writeIPCErr(conn, e)
			return
		}
		writeIPC(conn, reply)
		return
	}

	var lr cliproto.ListSourcesReply
	if e := d.callServer(ctx, "GET", "/api/v1/sources", nil, &lr); e != nil {
		writeIPCErr(conn, e)
		return
	}
	d.refreshSourceCache(lr.Sources)

	// Aggregate per-pipe info from JetStream (route B). List stream metadata
	// once, then fetch latest payload previews only for non-empty streams.
	if err := d.ensureNATS(ctx); err != nil {
		if e, ok := err.(*cliproto.Error); ok {
			writeIPCErr(conn, e)
		} else {
			writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		}
		return
	}
	js, e := d.jetStream()
	if e != nil {
		writeIPCErr(conn, e)
		return
	}
	accountID, err := uuid.Parse(d.State.AccountID())
	if err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: "bad org id"})
		return
	}

	enriched, err := enrichSourcesWithPipeInfo(ctx, js, lr.Sources, accountID, req.Session, nil, cursorSnapshot(d.Cursors, req.Session))
	if err != nil {
		writeIPCErr(conn, cliproto.New(cliproto.ENATSUnreachable))
		return
	}

	// Phase 1.5: append uncollared (sourceless) pipes. Walking sources
	// alone misses them; the server's GET /api/v1/pipes lists them
	// explicitly.
	var ucReply cliproto.ListUncollaredPipesReply
	if e := d.callServer(ctx, "GET", "/api/v1/pipes", nil, &ucReply); e != nil {
		writeIPCErr(conn, e)
		return
	}
	uncollared := make([]cliproto.UncollaredPipe, 0, len(ucReply.Pipes))
	for _, p := range ucReply.Pipes {
		info := uncollaredPipeInfo(ctx, js, accountID, p.Manifold, p.Name, req.Session, d.Cursors)
		info.CreatedBy = p.CreatedBy
		uncollared = append(uncollared, cliproto.UncollaredPipe{
			Manifold: p.Manifold,
			Name:     p.Name,
			Info:     info,
		})
	}

	writeIPC(conn, cliproto.ListReply{Sources: enriched, UncollaredPipes: uncollared})
}

// handleComplete is the lean read-only counterpart to handleList that
// backs `ppz __complete` (the shell-tab hot path). It serves from the
// in-memory completion cache populated by refreshSourceCache — no
// NATS, no JetStream, and at most ONE cheap HTTP GET on a cold daemon
// just to warm the cache. handleList by contrast pays two HTTP RTTs
// to the server PLUS N JetStream Info()s and previews per pipe; that
// was every tab press, hence the slowness.
//
// Failure modes are intentionally quiet: a logged-out daemon returns
// Stale with no sources rather than an error, because tab completion
// must never surface daemon-side problems to the user mid-keystroke.
func (d *Daemon) handleComplete(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.CompleteRequest
	_ = json.Unmarshal(params, &req) // optional body

	if _, ok := d.State.Credentials(); !ok {
		// Not logged in — nothing to complete against. Return a clean
		// empty Stale reply so the shell shows no suggestions and the
		// user isn't confused with an error mid-tab.
		writeIPC(conn, cliproto.CompleteReply{Stale: true})
		return
	}

	// Warm the cache on cold daemon. Single cheap HTTP probe;
	// JetStream stays out of this path. Subsequent tabs hit the
	// in-memory snapshot.
	//
	// Concurrent tab presses on a cold daemon would otherwise all
	// observe !ok and race to fire GET /api/v1/sources. The lock +
	// re-check coalesces them into one probe per cold start. The
	// fast path (warm cache) skips the lock entirely.
	if _, ok := d.State.CompletionSnapshot(); !ok {
		d.completionWarmMu.Lock()
		if _, ok := d.State.CompletionSnapshot(); !ok {
			var lr cliproto.ListSourcesReply
			// Errors are swallowed: tab completion's contract is "best
			// effort". A failed probe leaves the cache cold so the NEXT
			// caller retries — see completionWarmMu in daemon.go for
			// why this isn't sync.Once.
			if e := d.callServer(ctx, "GET", "/api/v1/sources", nil, &lr); e == nil {
				d.refreshSourceCache(lr.Sources)
			}
		}
		d.completionWarmMu.Unlock()
	}

	snapshot, loaded := d.State.CompletionSnapshot()
	writeIPC(conn, cliproto.CompleteReply{Sources: snapshot, Stale: !loaded})
}

// uncollaredPipeInfo gathers JetStream stats for one uncollared pipe.
// Mirrors the per-pipe enrichment in enrichSourcesWithPipeInfo but
// scoped to a sourceless stream. Phase 1.5.
func uncollaredPipeInfo(ctx context.Context, js jetstream.JetStream, accountID uuid.UUID, manifold, name, session string, cursors *cursors) cliproto.PipeInfo {
	info := cliproto.PipeInfo{Pipe: cliproto.FormatPipePath(manifold, "", name)}
	stream, err := js.Stream(ctx, natsubj.BuildStreamName(accountID, manifold, "", name))
	if err != nil {
		return info
	}
	sInfo, err := stream.Info(ctx)
	if err != nil {
		return info
	}
	info.Total = sInfo.State.Msgs
	info.LastSeq = sInfo.State.LastSeq
	// Cursor key matches handleRead's uncollared form. effectiveCursor
	// resets a watermark stamped against a prior incarnation of this
	// stream (pipe recreated) — see list_snapshot.go for rationale.
	cursorKey := uncollaredCursorKey(natsubj.BuildSubject(accountID, manifold, "", name))
	cursor := effectiveCursor(cursors.GetEntry(session, cursorKey), createdNanos(sInfo.Created), sInfo.State.LastSeq)
	if sInfo.State.LastSeq > cursor {
		// Cap at buffered count so purged messages don't strand
		// the unread counter — see list_snapshot.go for rationale.
		info.Unread = min(sInfo.State.LastSeq-cursor, sInfo.State.Msgs)
	}
	// Best-effort preview/last-payload: fetch the last message if any.
	if sInfo.State.LastSeq > 0 {
		if rawMsg, err := stream.GetMsg(ctx, sInfo.State.LastSeq); err == nil && rawMsg != nil {
			if env, perr := envelope.Unmarshal(rawMsg.Data); perr == nil {
				info.Payload = env.Payload
				info.Preview = cliproto.TruncatePayload(env.Payload)
				if !rawMsg.Time.IsZero() {
					t := rawMsg.Time
					info.LastAt = &t
				}
			}
		}
	}
	return info
}

// pipesForKind mirrors db.Source.Pipes() at the daemon level so we don't
// import internal/db just for this helper. Sorted alphabetically so ls
// output is deterministic. `broadcast` was removed pre-launch (locked
// decision #16).
func pipesForKind(kind string) []string {
	if kind == string(cliproto.KindPTY) {
		return []string{"heartbeat", "inbox", "stdctrl", "stdin", "stdout"}
	}
	return []string{"inbox"}
}

func daemonCursorKey(accountID uuid.UUID, handle, pipe string) string {
	return CursorKey(accountID.String(), handle, pipe)
}

func (d *Daemon) handleSubscribe(ctx context.Context, conn net.Conn, _ json.RawMessage) {
	// Phase 1: not used by any of the 35 tests. Return a clean error so the
	// CLI surface stays honest.
	writeIPCErr(conn, &cliproto.Error{Code: "E_NOT_IMPLEMENTED", Message: "Subscribe not implemented in Phase 1"})
	_ = ctx
	_ = fmt.Sprint
}
