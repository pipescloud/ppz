package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/envelope"
)

// v0.25.0 §4: when the recipient's `ppz read` consumes a message that
// has AckRequested=true, the recipient's daemon publishes an `ack:read`
// envelope back to the *original sender's* inbox.

// shouldEmitAck encapsulates the three guards listed in spec §4:
//  1. AckRequested must be true on the original.
//  2. Original Sender must be non-empty (else there's no destination
//     to send the ack back to).
//  3. Original Subject must NOT start with `ack:` (loop guard — belt
//     and suspenders since acks have AckRequested=false by default).
func TestShouldEmitAck_AllGuards(t *testing.T) {
	cases := []struct {
		name string
		msg  cliproto.ReadMessage
		want bool
	}{
		{
			name: "ack requested + sender + non-ack subject → emit",
			msg:  cliproto.ReadMessage{Sender: "alpha", Subject: "", AckRequested: true},
			want: true,
		},
		{
			name: "ack not requested → no emit",
			msg:  cliproto.ReadMessage{Sender: "alpha", Subject: "", AckRequested: false},
			want: false,
		},
		{
			name: "ack requested but headless original (empty sender) → no emit",
			msg:  cliproto.ReadMessage{Sender: "", Subject: "", AckRequested: true},
			want: false,
		},
		{
			name: "ack requested but already an ack (loop guard) → no emit",
			msg:  cliproto.ReadMessage{Sender: "alpha", Subject: "ack:read", AckRequested: true},
			want: false,
		},
		{
			name: "ack: prefix in subject blocks even with ack requested",
			msg:  cliproto.ReadMessage{Sender: "alpha", Subject: "ack:processed", AckRequested: true},
			want: false,
		},
		// Subject substrings that contain "ack:" but don't start with it are NOT system messages.
		{
			name: "ack: not at start of subject → still emit",
			msg:  cliproto.ReadMessage{Sender: "alpha", Subject: "user:ack:read", AckRequested: true},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldEmitAck(c.msg); got != c.want {
				t.Fatalf("shouldEmitAck(%+v) = %v, want %v", c.msg, got, c.want)
			}
		})
	}
}

// buildAckEnvelope is what the daemon stamps into NATS for `ack:read`.
//
//   - sender = the reading daemon's own current source (`self`); empty
//     string is permitted (rendered as `-` by the tabular formatter on
//     the original sender's side).
//   - subject = "ack:read" (system protocol subject).
//   - in_reply_to = the original message's ID, so the original sender
//     can correlate it.
//   - ack_requested = false on the ack itself (so receiving the ack
//     doesn't trigger an ack-of-ack).
//   - payload = "" (acks carry no body — the formatter renders them as
//     `ack:read → <id8>`).
func TestBuildAckEnvelope(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	original := cliproto.ReadMessage{
		ID:           "11111111-2222-3333-4444-555566667777",
		Sender:       "miner-test", // original publisher
		Subject:      "",
		Payload:      "long original body...",
		AckRequested: true,
	}
	ack := buildAckEnvelope(original, "sheriff", now)

	if ack.Sender != "sheriff" {
		t.Errorf("ack.Sender = %q, want sheriff (the reader's self)", ack.Sender)
	}
	if ack.Subject != "ack:read" {
		t.Errorf("ack.Subject = %q, want ack:read", ack.Subject)
	}
	if ack.InReplyTo != original.ID {
		t.Errorf("ack.InReplyTo = %q, want original id %q", ack.InReplyTo, original.ID)
	}
	if ack.AckRequested {
		t.Errorf("ack.AckRequested = true; should be false to prevent ack-of-ack recursion")
	}
	if ack.Payload != "" {
		t.Errorf("ack.Payload = %q, want empty", ack.Payload)
	}
	if !ack.CreatedAt.Equal(now) {
		t.Errorf("ack.CreatedAt = %v, want %v", ack.CreatedAt, now)
	}
	if ack.ID == "" {
		t.Errorf("ack.ID empty — must be a fresh uuid")
	}
	// System acks never jump the queue: priority stays 0 (unset ≡ medium)
	// even when the original message was high-priority. Pinned so a future
	// refactor can't silently make acks inherit priority.
	if ack.Priority != 0 {
		t.Errorf("ack.Priority = %d, want 0 (acks are metadata, not prioritised)", ack.Priority)
	}
}

func TestBuildAckEnvelope_IgnoresOriginalPriority(t *testing.T) {
	original := cliproto.ReadMessage{
		ID:           "11111111-2222-3333-4444-555566667777",
		Sender:       "miner-test",
		AckRequested: true,
		Priority:     1,
	}
	ack := buildAckEnvelope(original, "sheriff", time.Now())
	if ack.Priority != 0 {
		t.Errorf("ack.Priority = %d, want 0 even for a high-priority original", ack.Priority)
	}
}

// Reader's self="" is NOT a guard — the ack still publishes (with empty
// Sender) and routes correctly to the original sender's inbox. Spec §4
// "Reader's self == \"\" is NOT a guard."
func TestBuildAckEnvelope_AllowsEmptySelf(t *testing.T) {
	original := cliproto.ReadMessage{
		ID:           "11111111-2222-3333-4444-555566667777",
		Sender:       "miner-test",
		Subject:      "",
		AckRequested: true,
	}
	ack := buildAckEnvelope(original, "", time.Now())
	if ack.Sender != "" {
		t.Errorf("ack.Sender = %q, want empty (preserves self=\"\" — formatter renders as `-`)", ack.Sender)
	}
	if ack.Subject != "ack:read" {
		t.Errorf("ack.Subject = %q, want ack:read", ack.Subject)
	}
}

// emitAcks is the per-batch driver invoked after cursor advance in
// handleRead. For each message that passes shouldEmitAck, it publishes
// an ack to msg.Sender.inbox using the supplied publish func. Cursor
// advancement is the caller's job (and unconditional); emitAcks is
// pure ack-emission. Failed publishes are best-effort: they MUST NOT
// abort the loop.
func TestEmitAcks_FireAndForget_ContinuesPastFailures(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	accountID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	retained := []cliproto.ReadMessage{
		{ID: "msg-1", Sender: "alpha", AckRequested: true},
		{ID: "msg-2", Sender: "beta", AckRequested: true},  // publish for this one will fail
		{ID: "msg-3", Sender: "gamma", AckRequested: true}, // must still publish
	}

	type publishCall struct {
		dest, pipe string
		env        envelope.Message
	}
	var calls []publishCall
	publish := func(o uuid.UUID, dest, pipe string, env envelope.Message) error {
		calls = append(calls, publishCall{dest, pipe, env})
		if dest == "beta" {
			return errors.New("simulated publish failure")
		}
		return nil
	}

	emitAcks(accountID, "sheriff", retained, now, publish)

	if len(calls) != 3 {
		t.Fatalf("expected 3 publish attempts (one per message), got %d", len(calls))
	}
	if calls[0].dest != "alpha" || calls[1].dest != "beta" || calls[2].dest != "gamma" {
		t.Fatalf("publish destinations = %v %v %v, want alpha beta gamma",
			calls[0].dest, calls[1].dest, calls[2].dest)
	}
	for _, c := range calls {
		if c.pipe != "inbox" {
			t.Errorf("ack pipe = %q, want inbox (acks go to sender's .inbox)", c.pipe)
		}
		if c.env.Subject != "ack:read" {
			t.Errorf("ack subject = %q, want ack:read", c.env.Subject)
		}
	}
	// The failed publish to beta did NOT abort the loop — calls[2] still
	// ran. (`Best-effort, fire-and-forget`, spec §4.)
}

// emitAcks must NOT touch messages that fail any of the three guards.
func TestEmitAcks_SkipsGuardedMessages(t *testing.T) {
	retained := []cliproto.ReadMessage{
		{ID: "1", Sender: "alpha", AckRequested: true},                          // emit
		{ID: "2", Sender: "alpha", AckRequested: false},                         // no — flag clear
		{ID: "3", Sender: "", AckRequested: true},                               // no — empty sender
		{ID: "4", Sender: "alpha", Subject: "ack:read", AckRequested: true},     // no — loop guard
	}
	var dests []string
	publish := func(accountID uuid.UUID, dest, pipe string, env envelope.Message) error {
		dests = append(dests, dest)
		return nil
	}
	emitAcks(uuid.New(), "sheriff", retained, time.Now(), publish)

	if len(dests) != 1 || dests[0] != "alpha" {
		t.Fatalf("emitted destinations = %v, want exactly [alpha]", dests)
	}
}
