package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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

// auditPipe records one pipe-lifecycle mutation made through an API key.
func (s *Server) auditPipe(ctx context.Context, key db.APIKey, action, target string, before, after []byte) {
	userID, keyID := auditActorFromKey(key)
	s.recordAudit(ctx, db.AuditEvent{
		AccountID:     key.AccountID,
		ActorUserID:   userID,
		ActorAPIKeyID: keyID,
		Action:        action,
		TargetType:    db.AuditTargetPipe,
		Target:        target,
		Before:        before,
		After:         after,
	})
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
	// Every writer today is an API-key handler, so "web" is not yet
	// reachable — retention is only mutable through the CLI. It is here
	// because the GUI editor is the next step, and the honest reading of
	// a nil actor key is "not a key", not "assume a key".
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
			Delta:  formatRetentionDelta(ev.Before, ev.After),
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

// auditACL records one access-control change.
//
// Unlike auditPipe this takes the AuthedCaller rather than a db.APIKey,
// because the ACL routes are mounted on requireBearer: the caller may
// have arrived on a session token with no key at all, in which case
// ActorAPIKeyID stays nil and the GUI renders it as a person rather than
// "via key".
func (s *Server) auditACL(ctx context.Context, accountID uuid.UUID, caller AuthedCaller, action, targetType, target string, before, after []byte) {
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
