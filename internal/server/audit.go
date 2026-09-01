package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
)

// retentionSnapshot is the jsonb payload stored in an audit row's
// before/after columns. Deliberately the resolved triple rather than the
// raw nullable columns: the trail should say what the pipe actually
// retained, not which overrides happened to be set.
type retentionSnapshot struct {
	TTLSeconds int   `json:"ttl_seconds"`
	MaxMsgs    int   `json:"max_msgs"`
	MaxBytes   int64 `json:"max_bytes"`
}

func snapshotRetention(age time.Duration, msgs int, bytes int64) retentionSnapshot {
	return retentionSnapshot{
		TTLSeconds: int(age / time.Second),
		MaxMsgs:    msgs,
		MaxBytes:   bytes,
	}
}

// mustJSON marshals for storage. A fixed three-scalar struct cannot fail
// to marshal, so an error here is a programming impossibility rather
// than a runtime condition worth threading through every call site.
func (r retentionSnapshot) mustJSON() []byte {
	b, err := json.Marshal(r)
	if err != nil {
		return nil
	}
	return b
}

// auditActorFromKey resolves the actor pair for an API-key request.
//
// The actor is the key's PRINCIPAL — who the key acts as. For an
// ordinary key that is its creator, preserving the original reasoning:
// the server cannot know who typed the command, so a shared org key
// attributes to whoever minted it.
//
// For a service-account key (ACL Phase 1) principal and creator differ,
// and the principal is the right answer — nobody typed anything, the bot
// genuinely IS the actor. Attributing its work to the human who minted
// its key would make the trail misleading exactly where it matters most:
// the reader sees a person taking an action they never took.
//
// Returning the key id alongside is what lets the GUI render "via key"
// rather than implying a person was at a keyboard.
func auditActorFromKey(key db.APIKey) (uuid.UUID, *uuid.UUID) {
	id := key.ID
	return key.Actor(), &id
}

// recordAudit appends one row, best-effort.
//
// BEST-EFFORT IS A DELIBERATE TRADEOFF, not an oversight. By the time
// this runs, the mutation has already committed to postgres and — for
// pipe set — already been applied to JetStream, two steps that were
// never atomic with each other either. Failing the request now would
// report an error for work that actually happened, which misleads the
// caller worse than a missing log line does.
//
// The known gap: a postgres blip drops a row from the trail and only the
// server log says so. Closing it means putting the audit insert in the
// same transaction as the row write, which needs tx-taking variants of
// InsertPipe/DeletePipe.
func (s *Server) recordAudit(ctx context.Context, ev db.AuditEvent) {
	if s == nil || s.Pool == nil {
		return
	}
	if err := db.InsertAuditEvent(ctx, s.Pool, ev); err != nil {
		log.Printf("audit: dropping %s on %s: %v", ev.Action, ev.Target, err)
	}
}

// auditViaKey records one mutation made through an API key.
func (s *Server) auditViaKey(ctx context.Context, key db.APIKey, action, targetType, target string, before, after []byte) {
	userID, keyID := auditActorFromKey(key)
	s.recordAudit(ctx, db.AuditEvent{
		AccountID:     key.AccountID,
		ActorUserID:   userID,
		ActorAPIKeyID: keyID,
		Action:        action,
		TargetType:    targetType,
		Target:        target,
		Before:        before,
		After:         after,
	})
}

// auditPipe records one pipe-lifecycle mutation made through an API key.
func (s *Server) auditPipe(ctx context.Context, key db.APIKey, action, target string, before, after []byte) {
	s.auditViaKey(ctx, key, action, db.AuditTargetPipe, target, before, after)
}

// auditSource records one source-lifecycle mutation made through an API
// key. Source management is CLI-only, so unlike the key and membership
// writers there is no session path to cover here.
func (s *Server) auditSource(ctx context.Context, key db.APIKey, action, target string, before, after []byte) {
	s.auditViaKey(ctx, key, action, db.AuditTargetSource, target, before, after)
}

// sourceKindPayload is the before/after body for a source row: what the
// source WAS. Kind is the only field that can change over a source's
// life, and it is the one that decides whether the handle can be driven
// as a terminal or only read.
func sourceKindPayload(kind db.SourceKind) []byte {
	return fieldPayload(map[string]string{"kind": string(kind)})
}

// formatAuditDelta picks the renderer that matches the action.
//
// The payload shape is per-action: a pipe event carries a retention
// snapshot, an ACL event carries a principal and a permission. Running
// one through the other's formatter does not fail — it silently reads
// missing fields as zero — so before this existed, `acl.grant` rendered
// as "ttl=0s, msgs=0, bytes=0", i.e. the trail claimed a grant had reset
// the pipe's retention. A misleading audit line is worse than none.
func formatAuditDelta(action string, before, after []byte) string {
	switch action {
	case db.AuditActionPipeCreate, db.AuditActionPipeSet, db.AuditActionPipeDestroy:
		return formatRetentionDelta(before, after)
	case db.AuditActionACLGrant, db.AuditActionACLRevoke:
		return formatACLGrantDelta(action, before, after)
	case db.AuditActionACLEnforce:
		return formatACLEnforceDelta(before, after)
	default:
		return formatFieldDelta(before, after)
	}
}

// formatFieldDelta renders the org-lifecycle payloads: flat objects of
// scalars — a kind, a role, a state.
//
// These share one renderer rather than getting a formatter each because
// the shape is genuinely uniform, and a formatter per action is exactly
// how retention ended up as the default branch above and rendered
// `acl.grant` as "ttl=0s, msgs=0, bytes=0". Being the DEFAULT is the
// point: a future writer that forgets to add a case here gets a
// truthful, if plain, line instead of a confidently wrong one.
//
// Follows the same two rules formatRetentionDelta does. Only fields that
// MOVED appear, because reciting the unchanged ones buries the change;
// and when one half is absent (a create has no before, a destroy no
// after) it states the payload instead of diffing against nothing.
//
// Never panics and never errors — an audit tab that 500s on one
// malformed row is worse than one that renders it blank.
func formatFieldDelta(before, after []byte) string {
	b, bok := parseFieldPayload(before)
	a, aok := parseFieldPayload(after)
	switch {
	case !bok && !aok:
		return ""
	case !bok:
		return fieldStatement(a)
	case !aok:
		return fieldStatement(b)
	}
	var parts []string
	for _, k := range unionKeys(b, a) {
		bv, bHas := b[k]
		av, aHas := a[k]
		switch {
		case bHas && aHas:
			if bv != av {
				parts = append(parts, k+" "+bv+" → "+av)
			}
		case aHas:
			parts = append(parts, k+"="+av)
		default:
			parts = append(parts, k+"="+bv)
		}
	}
	return strings.Join(parts, ", ")
}

// parseFieldPayload decodes a flat jsonb object into rendered scalars.
//
// Values are rendered from their RAW JSON rather than from a decoded
// `any`: Go would print a JSON number 5000 as "5e+03" and a list as
// "[broadcast inbox]", neither of which is what was stored. Strings are
// unquoted; everything else passes through as the JSON it is.
//
// A non-object payload (an array, a bare scalar, malformed bytes) is
// reported as absent rather than half-rendered.
func parseFieldPayload(raw []byte) (map[string]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false
	}
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		var str string
		if json.Unmarshal(v, &str) == nil {
			out[k] = str
			continue
		}
		out[k] = string(v)
	}
	return out, true
}

// fieldStatement renders the whole payload, used when there's no
// counterpart to diff against.
func fieldStatement(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

// unionKeys returns every key across both sides, sorted. Sorted because
// map iteration is randomised in Go: without it the same event renders
// differently per request, and a row nobody can quote or diff is not
// much of an audit trail.
func unionKeys(a, b map[string]string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	keys := make([]string, 0, len(a)+len(b))
	for _, m := range []map[string]string{a, b} {
		for k := range m {
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// fieldPayload marshals an ordered set of scalars for before/after.
// Values are strings because every org-lifecycle payload is one — a
// kind, a role, a status, a key prefix.
func fieldPayload(kv map[string]string) []byte {
	b, err := json.Marshal(kv)
	if err != nil {
		return nil
	}
	return b
}

// auditUsername resolves the name a membership row should target.
//
// Names, not uuids: a trail you have to join against `users` to read is
// a trail nobody reads. DisplayName strips the "<org>/" scope prefix
// service accounts are stored under, matching every other surface.
//
// Falls back to "unknown" rather than failing the audit — a row that
// names the action and loses the name still beats no row.
func auditUsername(ctx context.Context, pool *db.Pool, id uuid.UUID) string {
	u, err := db.GetUser(ctx, pool, id)
	if err != nil {
		return "unknown"
	}
	return u.DisplayName()
}

// rolePayload is the before/after body for a membership change.
func rolePayload(role OrgRole) []byte {
	return fieldPayload(map[string]string{"role": string(role)})
}

// invitePayload is the before/after body for an invite row. Status is
// the only thing about an invite that moves.
func invitePayload(status db.InviteStatus) []byte {
	return fieldPayload(map[string]string{"status": string(status)})
}

// auditInviteDecision records an accept or a decline.
//
// Filed against the ORG — that is whose membership just changed, and an
// org owner reading their own trail is the person who needs to see it —
// but attributed to the INVITEE, who is the one who actually acted. The
// two differ here in a way they do not for any other writer, which is
// exactly why the split is worth being explicit about.
func (s *Server) auditInviteDecision(ctx context.Context, inv db.Invite, actor uuid.UUID, accept bool) {
	after := db.InviteStatusDeclined
	action := db.AuditActionInviteDecline
	if accept {
		after = db.InviteStatusAccepted
		action = db.AuditActionInviteAccept
	}
	s.auditOrg(ctx, inv.AccountID, AuthedCaller{UserID: actor}, action,
		db.AuditTargetInvite, inv.InviteeUsername,
		invitePayload(db.InviteStatusPending), invitePayload(after))
}

// sourcePath is the manifold-qualified handle an audit row targets.
// Mirrors cliproto.FormatPipePath's joining so a source and the pipes
// under it address the same way.
func sourcePath(manifold, handle string) string {
	if manifold == "" {
		return handle
	}
	return manifold + "." + handle
}

// formatACLGrantDelta renders "+read for bar" / "-read for bar".
func formatACLGrantDelta(action string, before, after []byte) string {
	payload := after
	sign := "+"
	if action == db.AuditActionACLRevoke {
		payload, sign = before, "−"
	}
	var v struct {
		Principal string `json:"principal"`
		Perm      string `json:"perm"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &v) != nil || v.Principal == "" {
		return ""
	}
	perm := v.Perm
	if perm == "" || perm == "all" {
		perm = "all permissions"
	}
	return sign + perm + " for " + v.Principal
}

// formatACLEnforceDelta renders "off → on".
func formatACLEnforceDelta(before, after []byte) string {
	state := func(b []byte) string {
		var v struct {
			Enforced bool `json:"enforced"`
		}
		if len(b) == 0 || json.Unmarshal(b, &v) != nil {
			return "?"
		}
		if v.Enforced {
			return "on"
		}
		return "off"
	}
	return state(before) + " → " + state(after)
}

// formatRetentionDelta renders the human line the audit tab shows.
//
// Only fields that MOVED appear: a `pipe set --ttl` row that also recites
// the unchanged msgs and bytes buries the one thing that happened. When
// one half is absent (a create has no before, a destroy has no after) it
// states the whole retention instead of a delta.
//
// Never panics and never returns an error — an audit tab that 500s on one
// malformed row is worse than one that renders it blank.
func formatRetentionDelta(before, after []byte) string {
	b, bok := parseRetentionSnapshot(before)
	a, aok := parseRetentionSnapshot(after)
	switch {
	case !bok && !aok:
		return ""
	case !bok:
		return a.statement()
	case !aok:
		return b.statement()
	}
	var parts []string
	if b.TTLSeconds != a.TTLSeconds {
		parts = append(parts, fmt.Sprintf("ttl %s → %s", ttlString(b.TTLSeconds), ttlString(a.TTLSeconds)))
	}
	if b.MaxMsgs != a.MaxMsgs {
		parts = append(parts, fmt.Sprintf("msgs %d → %d", b.MaxMsgs, a.MaxMsgs))
	}
	if b.MaxBytes != a.MaxBytes {
		parts = append(parts, fmt.Sprintf("bytes %d → %d", b.MaxBytes, a.MaxBytes))
	}
	return strings.Join(parts, ", ")
}

func parseRetentionSnapshot(raw []byte) (retentionSnapshot, bool) {
	if len(raw) == 0 {
		return retentionSnapshot{}, false
	}
	var snap retentionSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return retentionSnapshot{}, false
	}
	return snap, true
}

// statement renders the full retention, used when there's no counterpart
// to diff against.
func (r retentionSnapshot) statement() string {
	return fmt.Sprintf("ttl=%s, msgs=%d, bytes=%d", ttlString(r.TTLSeconds), r.MaxMsgs, r.MaxBytes)
}

func ttlString(secs int) string {
	return (time.Duration(secs) * time.Second).String()
}

// auditPageLimit bounds what the org audit tab renders in one page. The
// table has no retention policy, so an unbounded read would eventually
// try to render an org's entire history.
const auditPageLimit = 200

// auditRow is one rendered line of the audit tab.
type auditRow struct {
	Action string // "pipe.set"
	Target string // "chat.archive"
	Actor  string // username the action is attributed to
	// Via is "api-key" or "web", derived from whether the event carries
	// an actor key. The distinction matters: on the API path Actor is the
	// KEY'S CREATOR, not necessarily who typed the command, so a shared
	// key attributes every change to whoever minted it. Surfacing "via
	// api-key" stops the row reading as stronger evidence than it is.
	//
	// "web" became reachable with the org-lifecycle writers: key,
	// membership and invite management are session-authed GUI flows, so
	// those rows carry no actor key and genuinely name a person at a
	// keyboard. Pipe, source and ACL rows still arrive via the CLI.
	Via     string
	KeyHint string // key prefix when Via is "api-key", else ""
	Delta   string // "msgs 5000 → 5"
	When    string // absolute UTC, so rows stay stable to quote
}

// buildAuditRows renders events for display, resolving actor usernames
// and key prefixes in one batch each.
func buildAuditRows(ctx context.Context, pool *db.Pool, events []db.AuditEvent) []auditRow {
	if len(events) == 0 {
		return nil
	}
	userIDs := make([]uuid.UUID, 0, len(events))
	for _, ev := range events {
		userIDs = append(userIDs, ev.ActorUserID)
	}
	usernames, err := db.UsernamesByIDs(ctx, pool, userIDs)
	if err != nil {
		// A failed name lookup must not blank the trail — the actions,
		// targets and deltas are still the point.
		usernames = map[uuid.UUID]string{}
	}

	rows := make([]auditRow, 0, len(events))
	for _, ev := range events {
		row := auditRow{
			Action: ev.Action,
			Target: ev.Target,
			Actor:  usernames[ev.ActorUserID],
			Via:    "web",
			Delta:  formatAuditDelta(ev.Action, ev.Before, ev.After),
			When:   ev.CreatedAt.UTC().Format(time.RFC3339),
		}
		if row.Actor == "" {
			row.Actor = "unknown"
		}
		if ev.ActorAPIKeyID != nil {
			row.Via = "api-key"
			row.KeyHint = ev.ActorAPIKeyID.String()
		}
		rows = append(rows, row)
	}
	return rows
}

// auditOrg records one org-level change: an ACL edit, a key, a member,
// a service account, an invite.
//
// Unlike auditPipe this takes the AuthedCaller rather than a db.APIKey,
// because these routes are mounted on requireBearer or requireSession:
// the caller may have arrived on a session with no key at all, in which
// case ActorAPIKeyID stays nil and the GUI renders it as a person rather
// than "via key". Key and membership management are GUI-only flows, so
// they are the first writers to actually produce that rendering.
func (s *Server) auditOrg(ctx context.Context, accountID uuid.UUID, caller AuthedCaller, action, targetType, target string, before, after []byte) {
	var keyID *uuid.UUID
	if caller.APIKey != nil {
		id := caller.APIKey.ID
		keyID = &id
	}
	s.recordAudit(ctx, db.AuditEvent{
		AccountID:     accountID,
		ActorUserID:   caller.Principal(),
		ActorAPIKeyID: keyID,
		Action:        action,
		TargetType:    targetType,
		Target:        target,
		Before:        before,
		After:         after,
	})
}

// aclGrantDelta is the before/after payload for a grant or revoke: who
// was named and what they were given. Kept minimal on purpose — the
// trail records the change, not a snapshot of everything the principal
// could reach, which is derived and would be stale the moment a pipe is
// created.
func aclGrantDelta(principal, perm string) []byte {
	b, err := json.Marshal(map[string]string{"principal": principal, "perm": perm})
	if err != nil {
		return nil
	}
	return b
}

func aclEnforceDelta(on bool) []byte {
	b, err := json.Marshal(map[string]bool{"enforced": on})
	if err != nil {
		return nil
	}
	return b
}
