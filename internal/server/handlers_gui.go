package server

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

//go:embed templates/*.html
var templateFS embed.FS

var tmpl = template.Must(template.ParseFS(templateFS, "templates/*.html"))

// resolveOrg accepts either a UUID or the org's unique name (slug). UUID
// parse is tried first; on failure we fall back to a name lookup. A
// UUID-shaped name would always be treated as an ID — names in practice are
// short alphanumerics, so this is fine.
func resolveOrg(ctx context.Context, pool *db.Pool, idOrSlug string) (db.Account, error) {
	if id, err := uuid.Parse(idOrSlug); err == nil {
		return db.GetAccount(ctx, pool, id)
	}
	return db.GetAccountByName(ctx, pool, idOrSlug)
}

// base returns a map pre-populated with fields available to every template.
func (s *Server) base() map[string]any {
	return map[string]any{"Version": s.Version}
}

// handleGUILanding renders the marketing landing at `/`. Logo + tagline
// + two animated terminal demos. No DB calls — fully static. The
// operator-facing org index lives at `/dashboard`.
func (s *Server) handleGUILanding(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "landing.html", s.base()); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) handleGUIIndex(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	uid := UserIDFromCtx(r.Context())
	orgs, err := db.ListAccountsForUser(ctx, s.Pool, uid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Phase 4: pending invitations for the logged-in user, joined
	// against the user's username (invites target a username, not a
	// user_id). Failure here doesn't block the dashboard — render
	// without the section and log via the standard 500 path only on
	// outright user-lookup failure.
	var invites []db.InviteWithAccount
	if user, err := db.GetUser(ctx, s.Pool, uid); err == nil {
		invites, _ = db.ListPendingInvitesForUsername(ctx, s.Pool, user.Username)
	}
	// Onboarding: if the user has no pipes anywhere they own/belong
	// to, render the get-started panel on the dashboard. Failure here
	// is non-fatal — false negatives just hide the panel from someone
	// who'd benefit from it, never blocks the page.
	hasAnyPipe, _ := db.UserHasAnyPipe(ctx, s.Pool, uid)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := s.base()
	data["Orgs"] = orgs
	data["Invites"] = invites
	data["HasNoPipes"] = !hasAnyPipe
	// SiteURL is the scheme+host the browser used to reach us. Shown
	// verbatim in the onboarding `ppz login <url>` step so the user
	// can copy/paste a working command — pointing at the site they're
	// looking at, not at a hard-coded localhost.
	data["SiteURL"] = siteURL(r)
	if err := tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// namespaceDisplay renders a manifold for the org pipes table's
// NAMESPACE column: empty (root) → "-", otherwise the manifold path
// verbatim. Mirrors the convention `ppz ls` uses in cliproto so the
// CLI and GUI surfaces stay aligned.
func namespaceDisplay(manifold string) string {
	if manifold == "" {
		return "-"
	}
	return manifold
}

// siteURL reconstructs the browser-facing origin (scheme://host) from
// the current request. Honours the X-Forwarded-Proto header so reverse-
// proxied https deployments render correctly.
func siteURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	return scheme + "://" + r.Host
}

func (s *Server) handleGUICreateOrg(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name required", 400)
		return
	}
	// Default owner to the currently-authenticated user — otherwise
	// the creator wouldn't see their own org on /dashboard (which is
	// scoped by ownership/membership). An explicit owner_user_id form
	// field still wins, for the rare case a tool needs to assign on
	// behalf of someone else.
	ownerID := UserIDFromCtx(r.Context())
	if v := strings.TrimSpace(r.FormValue("owner_user_id")); v != "" {
		parsed, err := uuid.Parse(v)
		if err != nil {
			http.Error(w, "owner_user_id is not a valid uuid", 400)
			return
		}
		ownerID = parsed
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	if _, err := db.InsertAccount(ctx, s.Pool, name, ownerID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Browser submit (Referer set) → redirect back; API client (no
	// Referer, e.g. test harness via curl -L) → plain 200 to avoid
	// the curl-follows-with-POST → 405 dance. See browserSubmit.
	browserSubmit(w, r)
}

// handleGUIOrgRedirect is the bare `/orgs/{id}` entry point. The page
// is split into three tabs (pipes / users / keys); we 303 the visitor
// to the default tab so deep-linking + refresh land them on the same
// place. Pipes wins as default — it's the operator's most-used view.
func (s *Server) handleGUIOrgRedirect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("id"))
	if err != nil {
		http.Error(w, "org not found", 404)
		return
	}
	http.Redirect(w, r, "/orgs/"+org.Name+"/pipes", http.StatusSeeOther)
}

// orgTabs lists the tab keys the GUI knows about. The router registers
// one route per entry; the handler validates against this set so only
// valid tabs render.
var orgTabs = map[string]bool{"pipes": true, "users": true, "keys": true, "audit": true}

// handleGUIOrgTab dispatches the org page for a given tab. The tab
// drives which section the template renders, but the same shared
// header + sub-nav always appears so the user can switch.
func (s *Server) handleGUIOrgTab(w http.ResponseWriter, r *http.Request) {
	tab := r.PathValue("tab")
	if !orgTabs[tab] {
		http.Error(w, "unknown tab", http.StatusNotFound)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("id"))
	if err != nil {
		http.Error(w, "org not found", 404)
		return
	}
	keys, err := db.ListAPIKeysForOrg(ctx, s.Pool, org.ID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Decorate keys with their creators' usernames so the template can
	// render `data-key-creator="<username>"` per row. One DB round-trip
	// for the whole list.
	type keyView struct {
		db.APIKey
		Creator string
	}
	creatorIDs := make([]uuid.UUID, 0, len(keys))
	for _, k := range keys {
		creatorIDs = append(creatorIDs, k.CreatedByUserID)
	}
	creators, err := db.UsernamesByIDs(ctx, s.Pool, creatorIDs)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	keyViews := make([]keyView, len(keys))
	for i, k := range keys {
		keyViews[i] = keyView{APIKey: k, Creator: creators[k.CreatedByUserID]}
	}
	sources, err := db.ListSourcesForOrg(ctx, s.Pool, org.ID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	type sourceRow struct {
		Namespace      string // manifold path, or "-" for root (mirrors `ppz ls`)
		Handle         string
		Pipe           string // broadcast / stdin / stdout / user-created
		PipeLink       string // /orgs/<slug>/sources/<handle>/pipes/<pipe>
		TerminalLink   string // /orgs/<slug>/sources/<handle>/terminal — only set on stdout rows that have data (a shared terminal)
		LastMessageAt  string // relative duration ("5 seconds ago" / "just now") or ""
		PayloadDisplay string // truncated to 60 chars or ""
		HasLastMessage bool
	}
	// One row per (source, pipe). The pipe set is the union of:
	//   - auto-provisioned pipes derived from kind (`src.Pipes()`)
	//   - user-created pipes in the `pipes` table
	// "Last message" + payload come from JetStream — per-pipe, not just the
	// broadcast mirror in postgres — so user pipes (and stdin/stdout for
	// pty sources) get the same freshness signal as broadcast.
	now := time.Now()
	rows := make([]sourceRow, 0, len(sources))
	for _, src := range sources {
		pipeSet := map[string]struct{}{}
		for _, p := range src.Pipes() {
			pipeSet[p] = struct{}{}
		}
		userPipes, err := db.ListPipesForSource(ctx, s.Pool, src.ID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		for _, up := range userPipes {
			pipeSet[up.Name] = struct{}{}
		}
		pipes := make([]string, 0, len(pipeSet))
		for p := range pipeSet {
			pipes = append(pipes, p)
		}
		sort.Strings(pipes)
		// Phase 3.5: per-org JS. If account not provisioned yet,
		// rows still render without last-message data.
		js, _ := s.JSFor(ctx, org.ID)
		for _, p := range pipes {
			row := sourceRow{
				Namespace: namespaceDisplay(src.Manifold),
				Handle:    src.Handle,
				Pipe:      p,
				PipeLink:  fmt.Sprintf("/orgs/%s/sources/%s/pipes/%s", org.Name, src.Handle, p),
			}
			streamName := natsubj.BuildStreamName(org.ID, src.Manifold, src.Handle, p)
			if js == nil {
				rows = append(rows, row)
				continue
			}
			if stream, err := js.Stream(ctx, streamName); err == nil {
				if info, ierr := stream.Info(ctx); ierr == nil && info.State.Msgs > 0 {
					if !info.State.LastTime.IsZero() {
						row.LastMessageAt = cliproto.RelativeTime(info.State.LastTime, now)
						row.HasLastMessage = true
					}
					if msg, mErr := stream.GetMsg(ctx, info.State.LastSeq); mErr == nil {
						if env, eErr := envelope.Unmarshal(msg.Data); eErr == nil {
							row.PayloadDisplay = cliproto.TruncatePayload(env.Payload)
						}
					}
				}
			}
			// Surface the live-terminal link on stdout rows that actually
			// have data — that's the signal someone's shared a terminal
			// here (vs. a never-used auto-provisioned pipe).
			if p == "stdout" && row.HasLastMessage {
				row.TerminalLink = fmt.Sprintf("/orgs/%s/sources/%s/terminal", org.Name, src.Handle)
			}
			rows = append(rows, row)
		}
	}
	// Phase 1.5: uncollared (sourceless) pipes live in the `pipes`
	// table with source_id IS NULL, so walking sources alone misses
	// them and the GUI table silently drops every row a user created
	// via `ppz pipe create <leaf>` at the account root.
	uncollared, err := db.ListUncollaredPipesForAccount(ctx, s.Pool, org.ID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if len(uncollared) > 0 {
		js, _ := s.JSFor(ctx, org.ID)
		for _, up := range uncollared {
			// URL routing still needs the combined `<manifold>.<leaf>`
			// form so the pipe-detail handler can split on the last
			// dot to recover (manifold, name). But the PIPE cell
			// shows just the leaf — the manifold fact moves into
			// the NAMESPACE column.
			pipePath := up.Name
			if up.Manifold != "" {
				pipePath = up.Manifold + "." + up.Name
			}
			row := sourceRow{
				Namespace: namespaceDisplay(up.Manifold),
				Handle:    "",
				Pipe:      up.Name,
				PipeLink:  fmt.Sprintf("/orgs/%s/pipes/%s", org.Name, pipePath),
			}
			if js != nil {
				streamName := natsubj.BuildStreamName(org.ID, up.Manifold, "", up.Name)
				if stream, err := js.Stream(ctx, streamName); err == nil {
					if info, ierr := stream.Info(ctx); ierr == nil && info.State.Msgs > 0 {
						if !info.State.LastTime.IsZero() {
							row.LastMessageAt = cliproto.RelativeTime(info.State.LastTime, now)
							row.HasLastMessage = true
						}
						if msg, mErr := stream.GetMsg(ctx, info.State.LastSeq); mErr == nil {
							if env, eErr := envelope.Unmarshal(msg.Data); eErr == nil {
								row.PayloadDisplay = cliproto.TruncatePayload(env.Payload)
							}
						}
					}
				}
			}
			rows = append(rows, row)
		}
	}

	owner, err := db.GetUser(ctx, s.Pool, org.OwnerUserID)
	if err != nil {
		http.Error(w, "owner lookup: "+err.Error(), 500)
		return
	}
	members, err := db.ListMembers(ctx, s.Pool, org.ID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Phase 4: pending invitations for this org. Only used on the
	// "users" tab — but cheap enough to fetch unconditionally so the
	// template can stay simple.
	allInvites, _ := db.ListInvitesForOrg(ctx, s.Pool, org.ID)
	pendingInvites := make([]db.Invite, 0, len(allInvites))
	for _, inv := range allInvites {
		if inv.Status == db.InviteStatusPending {
			pendingInvites = append(pendingInvites, inv)
		}
	}
	// Owner-only flag for showing the invite-create form on the users
	// tab. Non-owners see members but not the controls.
	isOwner := UserIDFromCtx(r.Context()) == org.OwnerUserID

	// The audit tab is owner-only: the trail names people and the API
	// keys they acted through, which is owner information rather than
	// something every member of the org should read. Gated here rather
	// than in the template so a non-owner gets a 403 instead of a page
	// with a silently empty section.
	var auditRows []auditRow
	if tab == "audit" {
		if !isOwner {
			http.Error(w, "owner only", http.StatusForbidden)
			return
		}
		events, err := db.ListAuditEventsForAccount(ctx, s.Pool, org.ID, auditPageLimit)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		auditRows = buildAuditRows(events, s.Pool, ctx)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := s.base()
	data["Org"] = org
	data["Keys"] = keyViews
	data["Sources"] = rows
	data["Owner"] = owner
	data["Members"] = members
	data["PendingInvites"] = pendingInvites
	data["IsOwner"] = isOwner
	data["AuditRows"] = auditRows
	data["ActiveTab"] = tab
	if err := tmpl.ExecuteTemplate(w, "org.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) handleGUICreateKey(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		http.Error(w, "label required", 400)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("id"))
	if err != nil {
		http.Error(w, "org not found", 404)
		return
	}
	// requireSession populated user_id on the request context.
	creator := UserIDFromCtx(r.Context())
	key, plaintext, err := db.InsertAPIKey(ctx, s.Pool, org.ID, creator, label)
	if err != nil {
		http.Error(w, fmt.Sprintf("create key: %v", err), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "key_created.html", map[string]any{
		"AccountID":     org.ID.String(),
		"AccountName":   org.Name,
		"KeyID":     key.ID.String(),
		"Plaintext": plaintext,
	}); err != nil {
		http.Error(w, err.Error(), 500)
	}
}
