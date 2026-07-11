package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/db"
)

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Health.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": s.Version,
		})
	})

	// API (auth required except for /auth/exchange which authenticates via body).
	mux.HandleFunc("POST /api/v1/auth/exchange", s.handleAuthExchange)
	// Key revocation: no auth (mirrors the existing GUI key-create flow,
	// which is also un-authed). When auth lands across the surface,
	// this can be folded into requireAuth too.
	mux.HandleFunc("POST /api/v1/keys/{id}/revoke", s.handleRevokeKey)
	// Account-scoped invite API. Bearer-authed; the handler layer
	// gates on OAuth user identity (API keys 403 because they're
	// account-scoped without a user). The /orgs/ URL prefix is
	// kept for now as a stable wire shape — renaming to /accounts/
	// requires also renaming the GUI's `/orgs/{slug}/...` page URLs
	// (invitee accept-links are bookmarkable and forwardable), so
	// the API URL and GUI URL must move together. Tracked for
	// Phase 2 (auth restructure) which already touches the GUI
	// heavily. *(See PR #41 review point #5.)*
	mux.HandleFunc("POST /api/v1/orgs/{slug}/invites", s.requireBearer(s.handleAPICreateInvite))
	mux.HandleFunc("GET /api/v1/orgs/{slug}/invites", s.requireBearer(s.handleAPIListInvitesForOrg))
	mux.HandleFunc("POST /api/v1/orgs/{slug}/invites/{id}/revoke", s.requireBearer(s.handleAPIRevokeInvite))
	mux.HandleFunc("GET /api/v1/invites", s.requireBearer(s.handleAPIListMyInvites))
	mux.HandleFunc("POST /api/v1/invites/{id}/accept", s.requireBearer(s.handleAPIAcceptInvite))
	mux.HandleFunc("POST /api/v1/invites/{id}/decline", s.requireBearer(s.handleAPIDeclineInvite))

	mux.HandleFunc("POST /api/v1/sources", s.requireAPIKey(s.handleCreateSource))
	mux.HandleFunc("GET /api/v1/sources", s.requireAPIKey(s.handleListSources))
	mux.HandleFunc("GET /api/v1/sources/{handle}", s.requireAPIKey(s.handleGetSource))
	mux.HandleFunc("DELETE /api/v1/sources/{handle}", s.requireAPIKey(s.handleDestroySource))
	mux.HandleFunc("POST /api/v1/sources/{handle}/pipes", s.requireAPIKey(s.handleCreatePipe))
	mux.HandleFunc("DELETE /api/v1/sources/{handle}/pipes/{name}", s.requireAPIKey(s.handleDestroyPipe))
	mux.HandleFunc("POST /api/v1/pipes", s.requireAPIKey(s.handleCreatePipeFullPath))
	mux.HandleFunc("GET /api/v1/pipes", s.requireAPIKey(s.handleListUncollaredPipes))
	mux.HandleFunc("DELETE /api/v1/pipes", s.requireAPIKey(s.handleDestroyUncollaredPipe))

	// Scheduled sends (docs/specs/schedule.md).
	mux.HandleFunc("POST /api/v1/schedules", s.requireAPIKey(s.handleCreateSchedule))
	mux.HandleFunc("GET /api/v1/schedules", s.requireAPIKey(s.handleListSchedules))
	mux.HandleFunc("DELETE /api/v1/schedules/{id}", s.requireAPIKey(s.handleDeleteSchedule))

	// Auth V2 Phase 2: device-flow endpoints + GUI verify page.
	mux.HandleFunc("POST /oauth/device/code", s.handleDeviceCode)
	mux.HandleFunc("GET /oauth/device/verify", s.requireSession(s.handleDeviceVerifyPage))
	mux.HandleFunc("POST /oauth/device/verify", s.requireSession(s.handleDeviceVerifySubmit))
	mux.HandleFunc("POST /oauth/device/token", s.handleDeviceToken)

	// GUI public routes (no auth).
	mux.HandleFunc("GET /{$}", s.handleGUILanding)
	mux.HandleFunc("GET /login", s.handleGUILogin)

	// Auth flow endpoints (publicly accessible; the flow itself is
	// the gate).
	mux.HandleFunc("GET /auth/github/start", s.handleAuthGitHubStart)
	mux.HandleFunc("GET /auth/github/callback", s.handleAuthGitHubCallback)
	mux.HandleFunc("POST /auth/logout", s.handleAuthLogout)
	mux.HandleFunc("POST /dev/login", s.handleDevLogin) // gated by s.DevLogin internally
	mux.HandleFunc("GET /dev/login", s.handleDevLogin)  // GET+?next= for one-click local login

	// Test-only: wipes JetStream state across every per-org account.
	// 404s in prod (DevLogin=false). Phase 3.5 introduced per-org
	// accounts; the old `nats` CLI cleanup in reset.sh can no longer
	// reach across them, so the cleanup lives here where the server
	// already holds connections to every account.
	mux.HandleFunc("POST /api/v1/admin/wipe", s.handleAdminWipe)
	mux.HandleFunc("POST /api/v1/admin/simulate-stale-operator", s.handleSimulateStaleOperator)

	// GUI authed routes — wrapped in requireSession.
	mux.HandleFunc("GET /dashboard", s.requireSession(s.handleGUIIndex))
	mux.HandleFunc("GET /me", s.requireSession(s.handleMe))
	mux.HandleFunc("POST /orgs", s.requireSession(s.handleGUICreateOrg))
	mux.HandleFunc("GET /orgs/{id}", s.requireSession(s.handleGUIOrgRedirect))
	mux.HandleFunc("GET /orgs/{id}/{tab}", s.requireSession(s.handleGUIOrgTab))
	mux.HandleFunc("POST /orgs/{id}/keys", s.requireSession(s.handleGUICreateKey))
	mux.HandleFunc("POST /orgs/{id}/keys/{kid}/revoke", s.requireSession(s.handleGUIRevokeKey))
	mux.HandleFunc("POST /users", s.requireSession(s.handleCreateUser))
	mux.HandleFunc("POST /orgs/{id}/members", s.requireSession(s.handleAddMember))
	mux.HandleFunc("POST /orgs/{id}/members/{uid}/remove", s.requireSession(s.handleRemoveMember))
	mux.HandleFunc("POST /orgs/{id}/invites", s.requireSession(s.handleGUICreateInvite))
	mux.HandleFunc("POST /orgs/{id}/invites/{iid}/revoke", s.requireSession(s.handleGUIRevokeInvite))
	mux.HandleFunc("POST /invites/{id}/accept", s.requireSession(s.handleGUIAcceptInvite))
	mux.HandleFunc("POST /invites/{id}/decline", s.requireSession(s.handleGUIDeclineInvite))
	mux.HandleFunc("GET /orgs/{id}/sources/{handle}/pipes/{pipe}", s.requireSession(s.handleGUIPipePage))
	// Sourceless (uncollared) pipe view. {pipe} is the dotted
	// "<manifold>.<name>" path the org pipes table surfaces; the
	// handler splits on the last dot to recover manifold + leaf.
	mux.HandleFunc("GET /orgs/{id}/pipes/{pipe}", s.requireSession(s.handleGUIUncollaredPipePage))
	// Web `ppz chat` console — roster + live chat pane over the org's
	// agents, inboxes and pipes. All four routes (page, snapshot, send,
	// live WS) are session-authed here and additionally membership-gated
	// in their handlers (resolveChatOrg), since /chat is a read+write
	// surface into the org's streams.
	mux.HandleFunc("GET /orgs/{id}/chat", s.requireSession(s.handleGUIChatPage))
	mux.HandleFunc("GET /orgs/{id}/chat/messages", s.requireSession(s.handleGUIChatMessages))
	mux.HandleFunc("GET /orgs/{id}/chat/roster", s.requireSession(s.handleGUIChatRoster))
	mux.HandleFunc("POST /orgs/{id}/chat/read", s.requireSession(s.handleGUIChatMarkRead))
	mux.HandleFunc("POST /orgs/{id}/chat/send", s.requireSession(s.handleGUIChatSend))
	// Add/remove uncollared pipes from the console (TUI `a` / `-` parity).
	mux.HandleFunc("POST /orgs/{id}/chat/pipes", s.requireSession(s.handleGUIChatAddPipe))
	mux.HandleFunc("DELETE /orgs/{id}/chat/pipes", s.requireSession(s.handleGUIChatRemovePipe))
	// Unlike the terminal WS (left un-authed as a punt), the chat WS is
	// session-gated: the browser handshake carries the session cookie, so
	// the follow is access-controlled AND the server can stamp `you`
	// correctly from the viewer's identity. The client also derives `you`
	// from data-me, so a future un-authed transport still labels correctly.
	mux.HandleFunc("GET /orgs/{id}/chat/ws", s.requireSession(s.handleGUIChatWS))
	mux.HandleFunc("GET /orgs/{id}/sources/{handle}/terminal", s.requireSession(s.handleGUITerminalPage))
	// WebSocket for terminal stream — leaving un-auth'd for now (RED phase
	// surfaced this; tighten to session-or-key auth in a follow-up).
	mux.HandleFunc("GET /orgs/{id}/sources/{handle}/terminal/ws", s.handleGUITerminalWS)

	// Static assets (logo, css, xterm, …) embedded into the binary.
	mux.Handle("GET /assets/", assetsHandler())

	return mux
}

// authedHandler receives the resolved API key for routes that need
// per-org write access. Auth V2 Phase 2 moved the resolution into
// `requireAPIKey` (auth_bearer.go), which is backed by the unified
// requireBearer.
type authedHandler func(w http.ResponseWriter, r *http.Request, key db.APIKey)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *cliproto.Error) {
	writeJSON(w, cliproto.HTTPStatus(e.Code), cliproto.HTTPError{Error: *e})
}

// readJSON decodes the body, returning a usage error on malformed input.
func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("malformed json")
	}
	return nil
}

// withTimeout returns a context for the request bounded by 10s — enough for
// a pgx query under load but bounded so a stuck client can't hang a worker.
func withTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 10_000_000_000) // 10s
}

// browserSubmit is the right ack for a successful POST that came either
// from a browser form (has Referer → redirect back so the user sees the
// updated page) or from an API client like curl/test harness (no Referer
// → plain 200 OK, no body, no redirect chain to chase). The latter
// matters because curl with `-X POST -L` reposts on a 303, so a
// redirect to a path without a POST handler bounces back as 405.
func browserSubmit(w http.ResponseWriter, r *http.Request) {
	if ref := r.Referer(); ref != "" {
		http.Redirect(w, r, ref, http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusOK)
}
