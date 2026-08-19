package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestActiveConnectionClose_TriggersBackgroundReconnect — RED for the
// self-heal gap visible in the 2026-06-15 diagnostics. When the server
// kicked the live connection (TCP reset → "nats: Authorization
// Violation" close) while the daemon was idle, nats.go's ClosedHandler
// only RECORDED the close — nothing rebuilt the connection until the
// next scheduled JWT rotation (OnRefreshed), which is up to ~4.5 min
// away when awake and far longer across sleep. reportNATSFailure →
// kickReconnect only fires from the publish/read/wait paths, i.e. when
// a command is actively running; a purely background close had no
// recovery trigger at all.
//
// Contract: a terminal close of the daemon's ACTIVE connection must
// itself kick the background reconnect — no IPC command, and without
// waiting for a refresh.
func TestActiveConnectionClose_TriggersBackgroundReconnect(t *testing.T) {
	url := startEmbeddedNATSURL(t)
	d := &Daemon{
		State:      NewState(t.TempDir()),
		NATSEvents: newNATSEventRing(natsEventRingCap),
		Follows:    newFollowRegistry(),
		NATSURL:    url,
		// Wire the REAL observe handlers so ClosedHandler fires into
		// recordNATSEvent exactly as a production connection does — the
		// seam under test is that path, not a synthetic event.
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

	// Establish the initial connection (the path a command would take).
	if err := d.rebuildNC("test-initial"); err != nil {
		t.Fatalf("initial rebuildNC: %v", err)
	}
	if !waitNCConnected(d, 3*time.Second) {
		t.Fatalf("initial connection never came up")
	}
	d.ncMu.Lock()
	first := d.NC
	d.ncMu.Unlock()

	// Simulate the server kicking the live connection: close it from
	// under the daemon. nats.go fires ClosedHandler — an explicit close
	// is terminal and does NOT auto-reconnect, mirroring the
	// auth-violation close. We deliberately do NOT call reportNATSFailure
	// or any IPC command: the close itself must drive recovery.
	first.Close()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		d.ncMu.Lock()
		nc := d.NC
		d.ncMu.Unlock()
		if nc != nil && nc != first && nc.IsConnected() {
			t.Cleanup(func() { nc.Close() })
			return // GREEN: reconnected to a fresh conn on its own
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("active-connection close did not trigger a background reconnect")
}

// TestSwapClose_DoesNotChurnReconnect — guard against the obvious
// over-correction: the routine JWT-rotation swap closes the OLD
// connection (a "closed" event for an already-replaced NC). That close
// must NOT be treated as a failure — the daemon is healthy on the new
// conn, so no reconnect churn should fire and the connection must stay
// up and stable.
func TestSwapClose_DoesNotChurnReconnect(t *testing.T) {
	url := startEmbeddedNATSURL(t)
	d := &Daemon{
		State:      NewState(t.TempDir()),
		NATSEvents: newNATSEventRing(natsEventRingCap),
		Follows:    newFollowRegistry(),
		NATSURL:    url,
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

	// Force a swap to a brand-new connection. swapNCLocked installs the
	// new conn, then closes the old one — which fires a "closed" event
	// for the old NC. That must be recognised as an expected close.
	newNC, err := d.dial(url, d.Refresh, d.recordNATSEvent)
	if err != nil {
		t.Fatalf("dial replacement: %v", err)
	}
	d.swapNC("test-swap", newNC)

	// Give any (incorrect) churn a chance to manifest, then assert the
	// connection is stable and is exactly the one we swapped in — a
	// spurious reconnect would have replaced it again.
	time.Sleep(500 * time.Millisecond)
	d.ncMu.Lock()
	cur := d.NC
	connected := cur != nil && cur.IsConnected()
	d.ncMu.Unlock()
	if !connected {
		t.Fatalf("connection not healthy after a routine swap close")
	}
	if cur != newNC {
		t.Fatalf("routine swap-close churned a reconnect: NC replaced unexpectedly")
	}
	t.Cleanup(func() { newNC.Close() })
}

// TestFailureCloseRecoversWithoutChurn locks the "one no-op, no churn"
// invariant for the recursive path recordNATSEvent("closed") →
// kickReconnect. A failure-close kicks recovery; recovery's own swap
// (and the post-flag-reset race on the original close handler) can fire
// further "closed" events that kick again. Two brakes keep this bounded:
// the reconnecting CAS suppresses nested kicks while the loop runs, and
// rebuildNC's generation check coalesces any kick that slips through
// into a no-op that produces NO new swap — so the recursion terminates.
//
// The load-bearing brake is the coalescing: if it regressed (e.g.
// rebuildNC always dialed), the trailing kick would swap → close →
// kick → swap forever. We assert the INVARIANT, not the interleaving
// (which is timing-dependent and would flake): after recovery, the swap
// count must stop growing and the connection must stay put. A churn
// regression makes the swap count climb without bound.
func TestFailureCloseRecoversWithoutChurn(t *testing.T) {
	natsURL := startEmbeddedNATSURL(t)
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

	// Failure-close: closes the live conn AND kicks the background
	// reconnect, exactly like a JetStream-op failure in production.
	// (The embedded server here is core-only — no JetStream — so under
	// the verify-before-close contract this still legitimately closes:
	// a JS-level probe against it fails, which is the real zombie shape.)
	d.reportNATSFailure(errors.New("test: simulated jetstream op timeout"))

	if !waitNCConnected(d, 5*time.Second) {
		t.Fatalf("did not recover after failure-close")
	}

	countSwaps := func() int {
		n := 0
		for _, e := range d.NATSEvents.Snapshot() {
			if e.Type == "swap" {
				n++
			}
		}
		return n
	}

	// Let any trailing closed-event kicks (recovery's swap-close, plus
	// the post-reset race on the original close) play out — many ×
	// reconnectBackoff — then assert convergence.
	d.ncMu.Lock()
	converged := d.NC
	d.ncMu.Unlock()
	before := countSwaps()
	time.Sleep(200 * time.Millisecond)
	after := countSwaps()

	if after != before {
		t.Fatalf("swap churn after recovery: %d → %d swaps (coalescing brake regressed)", before, after)
	}
	d.ncMu.Lock()
	stable := d.NC == converged && d.NC != nil && d.NC.IsConnected()
	d.ncMu.Unlock()
	if !stable {
		t.Fatalf("connection not stable after recovery — churned to a different conn")
	}
	t.Cleanup(func() {
		if d.NC != nil {
			d.NC.Close()
		}
	})
}
