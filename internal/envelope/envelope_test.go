package envelope

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// v0.23.0: envelope carries `sender` (the broadcasting source) instead
// of `handle` (the destination), plus an optional `subject` (header-line
// metadata — user-set or system-set "ack:..."). Sender / subject are
// empty when the publisher has none.
func TestNew_PopulatesSenderAndSubject(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	m := New("alpha", "status update", "hi", now)
	if m.Sender != "alpha" {
		t.Fatalf("Sender = %q, want %q", m.Sender, "alpha")
	}
	if m.Subject != "status update" {
		t.Fatalf("Subject = %q, want %q", m.Subject, "status update")
	}
	if m.Payload != "hi" {
		t.Fatalf("Payload = %q", m.Payload)
	}
	if !m.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v", m.CreatedAt)
	}
	if m.ID == "" {
		t.Fatalf("ID empty")
	}
}

func TestNew_EmptySenderAndSubject(t *testing.T) {
	m := New("", "", "p", time.Now())
	if m.Sender != "" {
		t.Fatalf("Sender = %q, want empty", m.Sender)
	}
	if m.Subject != "" {
		t.Fatalf("Subject = %q, want empty", m.Subject)
	}
}

func TestMarshal_OmitsHandleField(t *testing.T) {
	m := New("alpha", "", "hi", time.Now())
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, `"handle"`) {
		t.Fatalf("marshalled envelope still contains \"handle\": %s", s)
	}
	if !strings.Contains(s, `"sender":"alpha"`) {
		t.Fatalf("marshalled envelope missing sender: %s", s)
	}
}

// Subject MUST always serialise — even when empty — so receivers see a
// stable shape. Conversely, decoding must accept envelopes that omit
// the field entirely (legacy + future older publishers).
func TestMarshal_AlwaysIncludesSubject(t *testing.T) {
	m := New("alpha", "", "hi", time.Now())
	b, _ := m.Marshal()
	if !strings.Contains(string(b), `"subject":""`) {
		t.Fatalf("empty subject must still appear on the wire: %s", b)
	}
	m2 := New("alpha", "ack:read", "hi", time.Now())
	b2, _ := m2.Marshal()
	if !strings.Contains(string(b2), `"subject":"ack:read"`) {
		t.Fatalf("non-empty subject missing: %s", b2)
	}
}

// Old retained messages (pre-v0.23) carry "handle" but not "sender" /
// "subject". They must parse cleanly: handle silently dropped, the new
// fields zero-value to "".
func TestUnmarshal_LegacyHandleParsesCleanly(t *testing.T) {
	legacy := []byte(`{"id":"abc","handle":"chat","payload":"hi","created_at":"2026-05-07T12:00:00Z"}`)
	m, err := Unmarshal(legacy)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.ID != "abc" {
		t.Fatalf("ID = %q", m.ID)
	}
	if m.Payload != "hi" {
		t.Fatalf("Payload = %q", m.Payload)
	}
	if m.Sender != "" {
		t.Fatalf("Sender from legacy envelope = %q, want empty", m.Sender)
	}
	if m.Subject != "" {
		t.Fatalf("Subject from legacy envelope = %q, want empty", m.Subject)
	}
}

// v0.25.0: envelope grows two more always-serialised fields:
//   - in_reply_to: uuid of the message this one replies to ("" when none).
//   - ack_requested: bool, sender wants an `ack:read` auto-emitted by the
//     receiver's daemon when its cursor advances past this message.
//
// Wire-shape rule (§1 of the spec): same as sender/subject — always
// serialised, even when empty/false, so receivers see a stable shape.
func TestNew_DefaultsInReplyToAndAckRequested(t *testing.T) {
	m := New("alpha", "", "hi", time.Now())
	if m.InReplyTo != "" {
		t.Fatalf("InReplyTo default = %q, want empty", m.InReplyTo)
	}
	if m.AckRequested {
		t.Fatalf("AckRequested default = true, want false")
	}
}

func TestMarshal_AlwaysIncludesInReplyToAndAckRequested(t *testing.T) {
	m := New("alpha", "", "hi", time.Now())
	b, _ := m.Marshal()
	s := string(b)
	if !strings.Contains(s, `"in_reply_to":""`) {
		t.Fatalf("empty in_reply_to must appear on the wire: %s", s)
	}
	if !strings.Contains(s, `"ack_requested":false`) {
		t.Fatalf("false ack_requested must appear on the wire: %s", s)
	}

	m2 := New("alpha", "", "hi", time.Now())
	m2.InReplyTo = "abc-123"
	m2.AckRequested = true
	b2, _ := m2.Marshal()
	s2 := string(b2)
	if !strings.Contains(s2, `"in_reply_to":"abc-123"`) {
		t.Fatalf("populated in_reply_to missing from wire: %s", s2)
	}
	if !strings.Contains(s2, `"ack_requested":true`) {
		t.Fatalf("true ack_requested missing from wire: %s", s2)
	}
}

// Old retained messages (pre-v0.25) lack in_reply_to / ack_requested.
// They must parse cleanly with both fields zero-valued.
func TestUnmarshal_LegacyV24EnvelopeParsesCleanly(t *testing.T) {
	legacy := []byte(`{"id":"abc","sender":"alpha","subject":"","payload":"hi","created_at":"2026-05-07T12:00:00Z"}`)
	m, err := Unmarshal(legacy)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Sender != "alpha" || m.Payload != "hi" {
		t.Fatalf("legacy envelope decoded wrong: %+v", m)
	}
	if m.InReplyTo != "" {
		t.Fatalf("InReplyTo from legacy envelope = %q, want empty", m.InReplyTo)
	}
	if m.AckRequested {
		t.Fatalf("AckRequested from legacy envelope = true, want false")
	}
}

// Round-trip preserves both fields when populated.
func TestUnmarshal_V25RoundtripWithReplyAndAck(t *testing.T) {
	m := New("alpha", "", "reply payload", time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	m.InReplyTo = "11111111-2222-3333-4444-555566667777"
	m.AckRequested = true
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.InReplyTo != m.InReplyTo {
		t.Fatalf("InReplyTo roundtrip = %q, want %q", got.InReplyTo, m.InReplyTo)
	}
	if got.AckRequested != true {
		t.Fatalf("AckRequested roundtrip = %v, want true", got.AckRequested)
	}
	// Cross-check raw wire keys.
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["in_reply_to"] != m.InReplyTo {
		t.Fatalf("wire in_reply_to = %v, want %s", raw["in_reply_to"], m.InReplyTo)
	}
	if raw["ack_requested"] != true {
		t.Fatalf("wire ack_requested = %v, want true", raw["ack_requested"])
	}
}

// v0.51.0: envelope grows an always-serialised `priority` field:
// 1=high 2=medium 3=low; 0 = unset (legacy / sender didn't ask), which
// readers treat as medium. Same wire-shape rule as every other field —
// always serialised, even when zero.
func TestNew_DefaultsPriorityToUnset(t *testing.T) {
	m := New("alpha", "", "hi", time.Now())
	if m.Priority != 0 {
		t.Fatalf("Priority default = %d, want 0 (unset)", m.Priority)
	}
}

func TestMarshal_AlwaysIncludesPriority(t *testing.T) {
	m := New("alpha", "", "hi", time.Now())
	b, _ := m.Marshal()
	if !strings.Contains(string(b), `"priority":0`) {
		t.Fatalf("unset priority must still appear on the wire: %s", b)
	}
	m2 := New("alpha", "", "hi", time.Now())
	m2.Priority = 1
	b2, _ := m2.Marshal()
	if !strings.Contains(string(b2), `"priority":1`) {
		t.Fatalf("populated priority missing from wire: %s", b2)
	}
}

// Old retained messages (pre-priority) lack the field. They must parse
// cleanly with Priority zero-valued (unset).
func TestUnmarshal_LegacyPrePriorityEnvelopeParsesCleanly(t *testing.T) {
	legacy := []byte(`{"id":"abc","sender":"alpha","subject":"","payload":"hi","created_at":"2026-05-07T12:00:00Z","in_reply_to":"","ack_requested":false}`)
	m, err := Unmarshal(legacy)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Sender != "alpha" || m.Payload != "hi" {
		t.Fatalf("legacy envelope decoded wrong: %+v", m)
	}
	if m.Priority != 0 {
		t.Fatalf("Priority from legacy envelope = %d, want 0", m.Priority)
	}
}

// Round-trip preserves priority when populated.
func TestUnmarshal_PriorityRoundtrip(t *testing.T) {
	m := New("alpha", "", "urgent payload", time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	m.Priority = 1
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != 1 {
		t.Fatalf("Priority roundtrip = %d, want 1", got.Priority)
	}
	// Cross-check raw wire key.
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["priority"] != float64(1) {
		t.Fatalf("wire priority = %v, want 1", raw["priority"])
	}
}

func TestUnmarshal_NewShapeRoundtrip(t *testing.T) {
	m := New("alpha", "ack:read", "hi", time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sender != "alpha" {
		t.Fatalf("Sender = %q", got.Sender)
	}
	if got.Subject != "ack:read" {
		t.Fatalf("Subject = %q", got.Subject)
	}
	// Cross-check that the wire shape is what we expect.
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["handle"]; ok {
		t.Fatalf("wire envelope still has \"handle\" key: %v", raw)
	}
	if raw["sender"] != "alpha" {
		t.Fatalf("wire sender = %v, want alpha", raw["sender"])
	}
	if raw["subject"] != "ack:read" {
		t.Fatalf("wire subject = %v, want ack:read", raw["subject"])
	}
}
