package daemon

import (
	"testing"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// buildBroadcastEnvelope is the pure envelope-assembly used inside
// handleSend. Pulling it out means we can verify the v0.25.0 field
// plumbing without standing up NATS or the daemon's IPC plumbing.
func TestBuildBroadcastEnvelope_PlumbsAllFields(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	req := cliproto.SendRequest{
		Handle:       "foo",
		Channel:      "inbox",
		Payload:      "hi",
		MsgSubject:   "status update",
		InReplyTo:    "11111111-2222-3333-4444-555566667777",
		AckRequested: true,
	}
	env := buildBroadcastEnvelope(req, "alpha", now)

	if env.Sender != "alpha" {
		t.Errorf("Sender = %q, want alpha", env.Sender)
	}
	if env.Subject != "status update" {
		t.Errorf("Subject = %q, want status update", env.Subject)
	}
	if env.Payload != "hi" {
		t.Errorf("Payload = %q", env.Payload)
	}
	if env.InReplyTo != "11111111-2222-3333-4444-555566667777" {
		t.Errorf("InReplyTo = %q, want from SendRequest", env.InReplyTo)
	}
	if !env.AckRequested {
		t.Errorf("AckRequested lost; envelope did not pick it up from SendRequest")
	}
	if !env.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", env.CreatedAt, now)
	}
}

// AckRequested defaults to false (zero value of the request) — confirm
// the helper doesn't synthesise it.
func TestBuildBroadcastEnvelope_NoAckByDefault(t *testing.T) {
	env := buildBroadcastEnvelope(cliproto.SendRequest{Payload: "p"}, "alpha", time.Now())
	if env.AckRequested {
		t.Errorf("AckRequested should default to false")
	}
	if env.InReplyTo != "" {
		t.Errorf("InReplyTo should default to empty, got %q", env.InReplyTo)
	}
}

// Priority plumbs through from the request; unset stays 0 (no default
// stamped at send time — legacy, ack, and batch envelopes must all
// look identical on the wire).
func TestBuildBroadcastEnvelope_PlumbsPriority(t *testing.T) {
	env := buildBroadcastEnvelope(cliproto.SendRequest{Payload: "p", Priority: 1}, "alpha", time.Now())
	if env.Priority != 1 {
		t.Errorf("Priority = %d, want 1 from SendRequest", env.Priority)
	}
	unset := buildBroadcastEnvelope(cliproto.SendRequest{Payload: "p"}, "alpha", time.Now())
	if unset.Priority != 0 {
		t.Errorf("Priority = %d, want 0 (unset) when the request carries none", unset.Priority)
	}
}

// validSendPriority is the IPC trust-boundary rule in handleSend: {0,1,2,3}
// only. The CLI rejects bad values before IPC (belt), so this pure helper
// is the only way to test the daemon check — any raw IPC client (custom
// scripts, harness adapters) hits it.
func TestValidSendPriority(t *testing.T) {
	for _, ok := range []int{0, 1, 2, 3} {
		if !validSendPriority(ok) {
			t.Errorf("validSendPriority(%d) = false, want true", ok)
		}
	}
	for _, bad := range []int{-5, -1, 4, 7, 99} {
		if validSendPriority(bad) {
			t.Errorf("validSendPriority(%d) = true, want false", bad)
		}
	}
}
