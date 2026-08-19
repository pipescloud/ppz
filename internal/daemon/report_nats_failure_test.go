package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// These tests pin the verify-before-close + attribution contract for
// reportNATSFailure, motivated by the 2026-08-19T01:44:25Z incident: a
// `ppz subs read` hit a JetStream stream-check timeout, reportNATSFailure
// amputated a demonstrably healthy connection, and the only trace in the
// event ring was an unattributed caller="nats.go" disconnect/closed pair
// with reason="" — indistinguishable from a network drop (nats.Conn.Close
// dispatches DisconnectedErrCB(nc, nil) then ClosedCB with LastError nil).
// ensureNATS rebuilt one second later; the network was never at fault.
//
// The revised contract (RED here, implemented in the GREEN change):
//
//  1. VERIFY: a JetStream-op timeout alone is not proof the connection is
//     dead — it can be one slow $JS.API reply. reportNATSFailure probes
//     JetStream responsiveness on the live conn first. Probe passes →
//     the connection is KEPT and a "warn" event records the report (so
//     the ring explains the caller's error even when nothing is closed).
//  2. CLOSE + ATTRIBUTE: only a failed probe (the true #116 zombie —
//     TCP/core alive, JetStream tier dead) closes the conn, and the
//     close records a "force_close" event carrying
//     Caller="reportNATSFailure" and the triggering error as Reason —
//     never again an orphan disconnect with no cause.

// startEmbeddedJSURL starts an embedded NATS server WITH JetStream and
// returns its client URL. URL-returning sibling of startEmbeddedJS (which
// returns a connected *nats.Conn) for tests that need the daemon to dial
// (and re-dial) the server itself.
func startEmbeddedJSURL(t *testing.T) string {
	t.Helper()
	s, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // ephemeral
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new embedded nats: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		s.Shutdown()
		t.Fatalf("embedded nats not ready")
	}
	t.Cleanup(s.Shutdown)
	return s.ClientURL()
}

// newReportFailureDaemon builds the standard failure-report test daemon:
// real observe handlers, a static fake refresh loop, and an established
// initial connection. Mirrors the closed_reconnect_test harness.
func newReportFailureDaemon(t *testing.T, natsURL string) (*Daemon, *nats.Conn) {
	t.Helper()
	d := &Daemon{
		State:            NewState(t.TempDir()),
		NATSEvents:       newNATSEventRing(natsEventRingCap),
		Follows:          newFollowRegistry(),
		Watches:          newWatchRegistry(),
		Heartbeats:       NewHeartbeatCache(),
		NATSURL:          natsURL,
		reconnectBackoff: 5 * time.Millisecond,
		dial: func(u string, _ *RefreshLoop, store func(NATSEvent)) (*nats.Conn, error) {
			return nats.Connect(u, natsObserveOptions(store, nil)...)
		},
	}
	loginForWakeTests(t, d)
	d.Refresh = &RefreshLoop{
		AccountID: "00000000-0000-0000-0000-000000000001",
		Refresh: func(context.Context, string) (string, string, int64, error) {
			return "jwt", "seed", time.Now().Add(5 * time.Minute).Unix(), nil
		},
	}
	if err := d.Refresh.Start(context.Background(), "jwt", "seed", time.Now().Add(5*time.Minute).Unix()); err != nil {
		t.Fatalf("RefreshLoop.Start: %v", err)
	}
	t.Cleanup(d.Refresh.Stop)

	if err := d.rebuildNC("test-initial"); err != nil {
		t.Fatalf("initial rebuildNC: %v", err)
	}
	if !waitNCConnected(d, 3*time.Second) {
		t.Fatalf("initial connection never came up")
	}
	d.ncMu.Lock()
	nc := d.NC
	d.ncMu.Unlock()
	t.Cleanup(func() {
		d.ncMu.Lock()
		cur := d.NC
		d.ncMu.Unlock()
		if cur != nil {
			cur.Close()
		}
	})
	return d, nc
}

// TestReportNATSFailure_KeepsHealthyConnection — the incident case. The
// server is healthy (embedded, JetStream enabled, zero latency); a
// JetStream op nonetheless reported a timeout (in production: one $JS.API
// reply slower than the 5s API timeout). The verification probe passes,
// so the connection must be KEPT — closing it here is what turned a
// latency blip into the 01:44:25Z drop for every concurrent command.
//
// RED: reportNATSFailure closes any connected NC unconditionally.
// GREEN: probe passes → same NC still installed and connected, no
// disconnect recorded for it, and a "warn" event attributes the report.
func TestReportNATSFailure_KeepsHealthyConnection(t *testing.T) {
	url := startEmbeddedJSURL(t)
	d, first := newReportFailureDaemon(t, url)
	firstID := ncID(first)
	mark := len(d.NATSEvents.Snapshot())

	cause := errors.New("nats: timeout")
	d.reportNATSFailure(cause)

	if !first.IsConnected() {
		t.Fatalf("reportNATSFailure closed a healthy connection — probe-before-close regressed: one slow JetStream reply must not drop the daemon's connection")
	}
	// Let any (erroneous) async teardown/rebuild play out, then confirm
	// the daemon is still on the SAME connection — no swap, no churn.
	time.Sleep(300 * time.Millisecond)
	d.ncMu.Lock()
	cur := d.NC
	d.ncMu.Unlock()
	if cur != first {
		t.Fatalf("daemon swapped connections after a passing probe — expected to keep %s", firstID)
	}
	window := d.NATSEvents.Snapshot()[mark:]
	for _, ev := range window {
		if (ev.Type == "disconnect" || ev.Type == "closed" || ev.Type == "force_close") && ev.NCID == firstID {
			t.Fatalf("healthy connection %s saw a %q event after a passing probe:%s", firstID, ev.Type, renderNATSEvents(window))
		}
	}
	// The report itself must still be visible in the ring: the caller
	// got an error, and diagnostics must be able to say why without the
	// connection having been touched.
	warn := findNATSEvent(window, "warn", firstID)
	if warn == nil || warn.Caller != "reportNATSFailure" {
		t.Fatalf("no warn event attributing the failure report (caller=reportNATSFailure, nc=%s); window:%s", firstID, renderNATSEvents(window))
	}
	if !strings.Contains(warn.Reason, cause.Error()) {
		t.Fatalf("warn reason %q does not carry the triggering error %q", warn.Reason, cause)
	}
}

// TestReportNATSFailure_ClosesZombieAndAttributesIt — the true #116
// zombie, and the attribution fix. The embedded server here is core-only
// (no JetStream): TCP and core NATS are alive, the JetStream tier is
// dead — exactly the JWT-expired-server-side shape #116 was built for.
// The probe fails, so the close must proceed AND be attributed: a
// "force_close" event with Caller="reportNATSFailure" and the triggering
// error as Reason, adjacent to the nats.go disconnect/closed pair it
// causes. Background recovery (kickReconnect) must still rebuild.
//
// RED: the close happens but records nothing — the ring shows only the
// orphan caller="nats.go" reason="" pair (the incident fingerprint).
// GREEN: the force_close event makes the close self-explaining.
func TestReportNATSFailure_ClosesZombieAndAttributesIt(t *testing.T) {
	url := startEmbeddedNATSURL(t) // core-only: JetStream tier "dead"
	d, first := newReportFailureDaemon(t, url)
	firstID := ncID(first)
	mark := len(d.NATSEvents.Snapshot())

	cause := errors.New("nats: timeout waiting for stream info")
	d.reportNATSFailure(cause)

	if first.IsConnected() {
		t.Fatalf("reportNATSFailure kept a zombie connection — probe against a dead JetStream tier must fail and close (the #116 contract)")
	}
	if !waitNCConnected(d, 5*time.Second) {
		t.Fatalf("background reconnect did not rebuild after the zombie close")
	}

	window := d.NATSEvents.Snapshot()[mark:]
	fc := findNATSEvent(window, "force_close", firstID)
	if fc == nil {
		t.Fatalf("zombie close is unattributed — no force_close event for %s; the ring shows only the orphan nats.go disconnect (the 2026-08-19 fingerprint):%s", firstID, renderNATSEvents(window))
	}
	if fc.Caller != "reportNATSFailure" {
		t.Fatalf("force_close caller = %q, want %q", fc.Caller, "reportNATSFailure")
	}
	if !strings.Contains(fc.Reason, cause.Error()) {
		t.Fatalf("force_close reason %q does not carry the triggering error %q", fc.Reason, cause)
	}
}

func findNATSEvent(events []NATSEvent, typ, ncid string) *NATSEvent {
	for i := range events {
		if events[i].Type == typ && events[i].NCID == ncid {
			return &events[i]
		}
	}
	return nil
}

func renderNATSEvents(events []NATSEvent) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("\n  ")
		b.WriteString(ev.At.Format("15:04:05.000"))
		b.WriteString(" " + ev.Type + " caller=" + ev.Caller + " nc=" + ev.NCID + " reason=" + ev.Reason)
	}
	return b.String()
}
