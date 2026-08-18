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
// The user is the key's CREATOR — the server genuinely cannot know who
// typed the command, so with a shared org key every change attributes to
// whoever minted the key. Returning the key id alongside is what lets the
// GUI render "via key" rather than implying a person was at a keyboard.
func auditActorFromKey(key db.APIKey) (uuid.UUID, *uuid.UUID) {
	id := key.ID
	return key.CreatedByUserID, &id
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
	// Via is "api-key" or "web". The distinction matters: on the API
	// path Actor is the KEY'S CREATOR, not necessarily who typed the
	// command, so a shared key attributes every change to whoever minted
	// it. Surfacing "via api-key" stops the row reading as stronger
	// evidence than it is.
	Via     string
	KeyHint string // key prefix when Via is "api-key", else ""
	Delta   string // "msgs 5000 → 5"
	When    string // absolute UTC, so rows stay stable to quote
}

// buildAuditRows renders events for display, resolving actor usernames
// and key prefixes in one batch each.
func buildAuditRows(events []db.AuditEvent, pool *db.Pool, ctx context.Context) []auditRow {
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
