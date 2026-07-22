package cli

import (
	"fmt"
	"testing"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// stoppedLeaseTimer returns a Timer that is not currently pending, matching how
// streamLeaseManagerOnce constructs the expiry timer it hands to
// handleLeaseMessage.
func stoppedLeaseTimer() *time.Timer {
	t := time.NewTimer(time.Hour)
	if !t.Stop() {
		<-t.C
	}
	return t
}

func leaseAcquireMsg(sender, nonce string, ttlMs int64, created time.Time) cliproto.ReadMessage {
	return cliproto.ReadMessage{
		Sender:    sender,
		CreatedAt: created.UTC().Format("2006-01-02T15:04:05Z"), // matches daemon read.go
		Payload:   fmt.Sprintf(`{"type":"lease-acquire","ttl_ms":%d,"nonce":%q}`, ttlMs, nonce),
	}
}

// TestHandleLeaseMessage_SkipsStaleAcquire reproduces the alice.system churn:
// a NoAdvance follow of .system makes JetStream redeliver retained acquires
// (on reconnect / ack-wait). An acquire that has outlived its TTL must NOT
// re-grant a phantom lease on redelivery — otherwise every redelivery re-grants
// the long-dead acquire, generating a lease-state burst forever.
func TestHandleLeaseMessage_SkipsStaleAcquire(t *testing.T) {
	t.Setenv("PPZ_IPC_SOCKET", "/nonexistent/ppz-stale-test.sock") // publish becomes a no-op
	lease := newLeaseState()
	timer := stoppedLeaseTimer()
	defer timer.Stop()

	// Acquired an hour ago with a 30s TTL — long stale.
	msg := leaseAcquireMsg("james", "stale1", 30_000, time.Now().Add(-time.Hour))
	handleLeaseMessage("alice", lease, msg, timer)

	if h := lease.holderAt(time.Now()); h != "" {
		t.Fatalf("stale acquire granted lease to %q, want no grant (stale acquires must be ignored)", h)
	}
}

// TestHandleLeaseMessage_GrantsFreshAcquire guards the other side: a current
// acquire (age ~0, as a live acquire/renewal has) must still be granted.
func TestHandleLeaseMessage_GrantsFreshAcquire(t *testing.T) {
	t.Setenv("PPZ_IPC_SOCKET", "/nonexistent/ppz-fresh-test.sock")
	lease := newLeaseState()
	timer := stoppedLeaseTimer()
	defer timer.Stop()

	msg := leaseAcquireMsg("james", "fresh1", 30_000, time.Now())
	handleLeaseMessage("alice", lease, msg, timer)

	if h := lease.holderAt(time.Now()); h != "james" {
		t.Fatalf("fresh acquire holder = %q, want james", h)
	}
}

// TestHandleLeaseMessage_GrantsShortTTLDespiteTimestampTruncation covers a real
// regression: created_at is emitted at second granularity (truncated), so a
// just-published acquire can carry a timestamp up to ~1s older than its real
// publish time. For a short TTL (`ppz terminal lease box 1s`) a plain age>ttl
// check would then falsely drop a VALID acquire — timing the caller out with
// E_LEASE_NO_HOST. The stale guard must include a grace margin so short-lived
// leases still grant. Here: a valid acquire whose truncated timestamp reads
// ~1.2s old under a 1s TTL must still be granted.
func TestHandleLeaseMessage_GrantsShortTTLDespiteTimestampTruncation(t *testing.T) {
	t.Setenv("PPZ_IPC_SOCKET", "/nonexistent/ppz-shortttl-test.sock")
	lease := newLeaseState()
	timer := stoppedLeaseTimer()
	defer timer.Stop()

	// TTL 1s; timestamp reads 1.2s old (truncation + processing), i.e. just
	// over the raw TTL but well within any sane grace window.
	msg := leaseAcquireMsg("james", "short1", 1_000, time.Now().Add(-1200*time.Millisecond))
	handleLeaseMessage("alice", lease, msg, timer)

	if h := lease.holderAt(time.Now()); h != "james" {
		t.Fatalf("short-TTL acquire holder = %q, want james (truncation must not falsely drop a valid acquire)", h)
	}
}
