package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
)

// The audit row's before/after payloads are the whole reason the tab is
// worth reading: "someone changed retention" is noise, "msgs 5000 → 5"
// is an answer. snapshotRetention produces the jsonb both sides agree on.

func TestSnapshotRetention_JSONShape(t *testing.T) {
	snap := snapshotRetention(24*time.Hour, 5000, 16777216)
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, want := range map[string]float64{
		"ttl_seconds": 86400,
		"max_msgs":    5000,
		"max_bytes":   16777216,
	} {
		v, ok := got[key]
		if !ok {
			t.Errorf("snapshot missing %q, got %s", key, b)
			continue
		}
		if v != want {
			t.Errorf("snapshot[%q] = %v, want %v", key, v, want)
		}
	}
	if len(got) != 3 {
		t.Errorf("snapshot has %d keys, want exactly 3: %s", len(got), b)
	}
}

// The API-key path records BOTH the user the key is attributed to and
// the key itself. Dropping the key id would make a shared-key change
// indistinguishable from that person acting in the web GUI.
func TestAuditActorFromKey_RecordsKeyID(t *testing.T) {
	key := db.APIKey{
		ID:              uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		AccountID:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		CreatedByUserID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
	}
	userID, keyID := auditActorFromKey(key)
	if userID != key.CreatedByUserID {
		t.Errorf("actor user = %v, want %v (the key's creator)", userID, key.CreatedByUserID)
	}
	if keyID == nil {
		t.Fatal("actor api key id = nil, want the key's ID (a shared key must not look like a web-session action)")
	}
	if *keyID != key.ID {
		t.Errorf("actor api key id = %v, want %v", *keyID, key.ID)
	}
}

// formatRetentionDelta renders the human line the audit tab shows. Only
// fields that actually MOVED appear — a `pipe set --ttl` row that also
// recites the unchanged msgs/bytes buries the one thing that happened.
func TestFormatRetentionDelta_OnlyChangedFields(t *testing.T) {
	before := mustSnapshotJSON(t, snapshotRetention(24*time.Hour, 5000, 16777216))
	after := mustSnapshotJSON(t, snapshotRetention(24*time.Hour, 5, 16777216))

	got := formatRetentionDelta(before, after)
	want := "msgs 5000 → 5"
	if got != want {
		t.Errorf("formatRetentionDelta = %q, want %q", got, want)
	}
}

func TestFormatRetentionDelta_MultipleFields(t *testing.T) {
	before := mustSnapshotJSON(t, snapshotRetention(24*time.Hour, 5000, 16777216))
	after := mustSnapshotJSON(t, snapshotRetention(time.Hour, 10, 16777216))

	got := formatRetentionDelta(before, after)
	want := "ttl 24h0m0s → 1h0m0s, msgs 5000 → 10"
	if got != want {
		t.Errorf("formatRetentionDelta = %q, want %q", got, want)
	}
}

// A create has no "before" and a destroy has no "after"; the renderer
// states the whole retention rather than a delta, and must not crash on
// the nil half.
func TestFormatRetentionDelta_HandlesMissingHalves(t *testing.T) {
	full := mustSnapshotJSON(t, snapshotRetention(24*time.Hour, 5000, 16777216))

	if got := formatRetentionDelta(nil, full); got != "ttl=24h0m0s, msgs=5000, bytes=16777216" {
		t.Errorf("create-shaped delta = %q, want the full retention statement", got)
	}
	if got := formatRetentionDelta(full, nil); got != "ttl=24h0m0s, msgs=5000, bytes=16777216" {
		t.Errorf("destroy-shaped delta = %q, want the full retention statement", got)
	}
	if got := formatRetentionDelta(nil, nil); got != "" {
		t.Errorf("both-nil delta = %q, want empty", got)
	}
}

// Malformed jsonb must not take the page down — an audit tab that 500s
// on one bad row is worse than one that renders it blank.
func TestFormatRetentionDelta_ToleratesGarbage(t *testing.T) {
	if got := formatRetentionDelta([]byte("{not json"), []byte("{also not")); got != "" {
		t.Errorf("garbage payloads = %q, want empty (never panic, never 500)", got)
	}
}

// recordAudit is best-effort BY DESIGN, and this test exists to make
// that tradeoff visible rather than accidental.
//
// The mutation it describes has already committed to postgres and, for
// pipe set, already been applied to JetStream — those two were never
// atomic with each other either. Failing the request now would report
// an error for work that actually happened, which is a worse lie than a
// missing log line. So a failed audit write is swallowed (and logged),
// and the KNOWN gap is that a postgres blip can drop a row from the
// trail without anyone being told.
func TestRecordAudit_NilPoolDoesNotPanic(t *testing.T) {
	srv := &Server{}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("recordAudit panicked with no pool: %v", r)
		}
	}()
	srv.recordAudit(context.Background(), db.AuditEvent{
		AccountID:   uuid.New(),
		ActorUserID: uuid.New(),
		Action:      db.AuditActionPipeSet,
		TargetType:  db.AuditTargetPipe,
		Target:      "chat.archive",
	})
}

func mustSnapshotJSON(t *testing.T, snap retentionSnapshot) []byte {
	t.Helper()
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return b
}
