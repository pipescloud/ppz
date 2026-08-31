package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// ACL Phase 0b — the presence subscription must not be an org firehose.
//
// subscribeOrgHeartbeats core-subscribes to "<account>.>" and filters
// for a ".heartbeat" suffix inside the callback
// (heartbeat_subscriber.go). Live JetStream publishes are also
// delivered to core subscribers, so every daemon holding that
// subscription receives every message published anywhere in the org —
// every stdout byte of every shared terminal, every inbox message
// between other agents.
//
// No JetStream permission can close that: the bytes arrive over a core
// subscription that never touches the JS API. Pipe read ACLs are
// therefore bypassable in one line of client code until presence moves
// to its own subject family and the wildcard narrows to match.
//
// subscribePresence replaces subscribeOrgHeartbeats. It returns the
// subscription so the scope of the fix is directly assertable rather
// than inferred from downstream behaviour.

// presenceDaemon wires a Daemon to the embedded server with just the
// fields the presence path touches.
func presenceDaemon(t *testing.T, url string) *Daemon {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return &Daemon{
		NC:         nc,
		NATSURL:    url,
		State:      NewState(t.TempDir()),
		NATSEvents: newNATSEventRing(natsEventRingCap),
		Heartbeats: NewHeartbeatCache(),
	}
}

func publishEnvelope(t *testing.T, nc *nats.Conn, subject, payload string) {
	t.Helper()
	raw, err := json.Marshal(envelope.New("test", subject, payload, time.Now()))
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := nc.Publish(subject, raw); err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// The assertion that closes the leak: the daemon holds a subscription
// scoped to presence, not to the whole account.
func TestSubscribePresence_ScopedNotOrgWide(t *testing.T) {
	acct := uuid.New()
	d := presenceDaemon(t, startEmbeddedNATSURL(t))

	sub, err := d.subscribePresence(acct)
	if err != nil {
		t.Fatalf("subscribePresence: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	if sub.Subject == natsubj.OrgSubscription(acct) {
		t.Fatalf("presence subscription is the org firehose %q — every message in the org is delivered to this daemon", sub.Subject)
	}
	if want := natsubj.PresencePrefix(acct); sub.Subject != want {
		t.Errorf("subscription subject = %q, want %q", sub.Subject, want)
	}
}

// Behavioural half of the same claim: pipe traffic published while the
// presence subscription is live must never reach the daemon.
func TestSubscribePresence_DoesNotSeePipeTraffic(t *testing.T) {
	acct := uuid.New()
	url := startEmbeddedNATSURL(t)
	d := presenceDaemon(t, url)

	sub, err := d.subscribePresence(acct)
	if err != nil {
		t.Fatalf("subscribePresence: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(pub.Close)

	// Traffic a principal with no read grant must never observe.
	for _, subj := range []string{
		natsubj.BuildSubject(acct, "", "alice", "stdout"),
		natsubj.BuildSubject(acct, "", "alice", "inbox"),
		natsubj.BuildSubject(acct, "ns", "agent-b", "stdout"),
		natsubj.BuildSubject(acct, "", "", "room"),
	} {
		publishEnvelope(t, pub, subj, "secret")
	}

	// Round-trip a presence message afterwards so we know delivery has
	// drained before asserting — otherwise an empty count proves only
	// that nothing has arrived yet.
	publishEnvelope(t, pub, natsubj.PresenceSubject(acct, "", "sentinel"), `{"seq":1}`)
	if !waitForHandle(d, "sentinel", 2*time.Second) {
		t.Fatal("sentinel presence message never arrived — subscription is not delivering at all")
	}

	if n, _, err := sub.Pending(); err == nil && n > 0 {
		t.Errorf("%d messages still pending on the presence subscription", n)
	}
	if delivered, err := sub.Delivered(); err == nil && delivered != 1 {
		t.Errorf("subscription delivered %d messages, want 1 (the sentinel) — pipe traffic is reaching this daemon", delivered)
	}
	for _, e := range d.Heartbeats.Snapshot() {
		if e.Handle != "sentinel" {
			t.Errorf("pipe traffic leaked into the presence cache as handle %q", e.Handle)
		}
	}
}

// Presence still works: a heartbeat published on the new subject lands
// in the cache that backs `ppz who`.
func TestSubscribePresence_StampsCache(t *testing.T) {
	acct := uuid.New()
	url := startEmbeddedNATSURL(t)
	d := presenceDaemon(t, url)

	sub, err := d.subscribePresence(acct)
	if err != nil {
		t.Fatalf("subscribePresence: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(pub.Close)

	publishEnvelope(t, pub, natsubj.PresenceSubject(acct, "", "alice"), `{"seq":7}`)

	if !waitForHandle(d, "alice", 2*time.Second) {
		t.Fatal("presence message never stamped the cache")
	}
	for _, e := range d.Heartbeats.Snapshot() {
		if e.Handle == "alice" && e.Payload != `{"seq":7}` {
			t.Errorf("payload = %q, want the inner heartbeat JSON", e.Payload)
		}
	}
}

// The bare-handle invariant. handleSend stamps the publisher's own
// cache with req.Handle (no manifold), so a subscriber that stamped
// "ns.agent-b" would render the same agent twice across daemons and
// break tests/agent/who-shows-cross-daemon-agents.
func TestSubscribePresence_StampsBareHandleForNamespacedAgent(t *testing.T) {
	acct := uuid.New()
	url := startEmbeddedNATSURL(t)
	d := presenceDaemon(t, url)

	sub, err := d.subscribePresence(acct)
	if err != nil {
		t.Fatalf("subscribePresence: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(pub.Close)

	publishEnvelope(t, pub, natsubj.PresenceSubject(acct, "ns", "agent-b"), `{"seq":1}`)

	if !waitForHandle(d, "agent-b", 2*time.Second) {
		t.Fatal("namespaced agent never stamped the cache under its bare handle")
	}
	for _, e := range d.Heartbeats.Snapshot() {
		if e.Handle == "ns.agent-b" {
			t.Fatal(`cache stamped under "ns.agent-b" — must be the bare handle "agent-b"`)
		}
	}
}

func waitForHandle(d *Daemon, handle string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, e := range d.Heartbeats.Snapshot() {
			if e.Handle == handle {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
