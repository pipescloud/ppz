package daemon

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// RED tests for the PR #193 review findings 1-4. Each pins one gap in
// the "login clears the heartbeat cache" fix.

// TestHandleLogin_ClearSurvivesLateOldOrgBeat — finding 1: Clear() must
// run AFTER swapNC closes the old connection, not before the dial. The
// old account's org-heartbeat subscription stays live through
// startRefreshLoop and the network dial; a beat it delivers in that
// window is stamped post-clear and then its subscription dies — a
// never-refreshing ghost, exactly the bug the fix targets. The dial
// stub stands in for that window: it stamps a beat the way the old
// subscription callback would, mid-login.
func TestHandleLogin_ClearSurvivesLateOldOrgBeat(t *testing.T) {
	srv := okExchangeServer(t, loginTestAcctNew)
	d := newLoginTestDaemon(t)
	loginAsPriorAccount(t, d, loginTestAcctOld)
	// Park the post-login recovery loop (kicked because the dial fails)
	// after its first retry: its re-dials model nothing here and must
	// not interleave with the assertion below.
	d.reconnectBackoff = time.Hour
	// Only the FIRST dial is the login window with the old subscription
	// still live — later dials (the background recovery loop) happen
	// after swapNC closed the old connection, when no old-org beat can
	// arrive, so they must not stamp (that re-stamp was a CI flake).
	var dials atomic.Int32
	d.dial = func(string, *RefreshLoop, func(NATSEvent)) (*nats.Conn, error) {
		if dials.Add(1) == 1 {
			d.Heartbeats.Stamp("ghost", `{"harness":"claude"}`, time.Now())
		}
		return nil, errors.New("dial window: connect refused")
	}

	if _, e := driveLogin(t, d, cliproto.LoginRequest{URL: srv.URL, APIKey: "ppz_oauth_test"}); e != nil {
		t.Fatalf("login returned error: %v", e)
	}

	if got := d.Heartbeats.Snapshot(); len(got) != 0 {
		t.Fatalf("old-org beat delivered during the dial window survived login (%v): "+
			"Clear() must run after swapNC, or the ghost outlives its subscription", got)
	}
}

// TestHandleLogin_SameAccountReloginKeepsHeartbeatCache — finding 3:
// re-running ppz login into the SAME org (key rotation, scripted
// re-auth) crosses no account boundary. The re-armed subscription keeps
// refreshing the same handles, so wiping the cache only buys a
// nobody-is-online hole in `ppz who` for up to a full heartbeat
// interval. The clear must be gated on the account actually changing.
func TestHandleLogin_SameAccountReloginKeepsHeartbeatCache(t *testing.T) {
	srv := okExchangeServer(t, loginTestAcctOld)
	d := newLoginTestDaemon(t)
	loginAsPriorAccount(t, d, loginTestAcctOld)

	d.Heartbeats.Stamp("alice", `{"harness":"claude"}`, time.Now())

	if _, e := driveLogin(t, d, cliproto.LoginRequest{URL: srv.URL, APIKey: "ppz_rotated_key"}); e != nil {
		t.Fatalf("login returned error: %v", e)
	}

	if got := d.Heartbeats.Snapshot(); len(got) != 1 || got[0].Handle != "alice" {
		t.Fatalf("same-account re-login must keep the heartbeat cache (rows stay live "+
			"under the re-armed subscription), got %v", got)
	}
}

// TestWatchState_LogoutClearsHeartbeatCache — finding 2: logout is an
// account-boundary crossing too. watchState notices the credentials
// file disappearing and drops the NATS connection, but left the
// heartbeat cache serving the logged-out account's agents to `ppz who`
// indefinitely — an unbounded sibling of the login bug. (The "who was
// running before my daemon got sick" affordance in handleWho is about
// broken connectivity, not a deliberate logout.)
func TestWatchState_LogoutClearsHeartbeatCache(t *testing.T) {
	d := newLoginTestDaemon(t)
	loginAsPriorAccount(t, d, loginTestAcctOld)
	d.Heartbeats.Stamp("alice", `{"harness":"claude"}`, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hupCh := make(chan os.Signal, 1)
	go d.watchState(ctx, hupCh)

	if err := os.Remove(d.State.Home() + "/" + fileCredentials); err != nil {
		t.Fatalf("removing credentials file: %v", err)
	}
	// SIGHUP forces a reload regardless of fileSig bookkeeping — the
	// deterministic trigger; a tick-based wait raced the watcher's first
	// 50ms stat (if removal won, lastCred stayed the zero fileSig, which
	// equals the post-removal stat forever, and the reload never fired).
	hupCh <- syscall.SIGHUP

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(d.Heartbeats.Snapshot()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("heartbeat cache still holds %v after logout: the logged-out account's "+
		"agents render in `ppz who` until a daemon restart", d.Heartbeats.Snapshot())
}

// TestHandleLogin_DialFailureKicksReconnect — finding 4: when the
// exchange succeeds but the best-effort NATS dial fails, handleLogin
// installs a nil connection and (post-fix) has cleared the old-account
// cache — so `ppz who` reports nobody. A pure dial failure emits no
// "closed" event and handleWho never calls ensureNATS, so nothing
// rebuilds until the JWT refresh fires (~4.5 min out). handleLogin must
// kick the single-flight background reconnect loop, which self-heals
// within its backoff cadence.
func TestHandleLogin_DialFailureKicksReconnect(t *testing.T) {
	url := startEmbeddedNATSURL(t)
	srv := okExchangeServer(t, loginTestAcctNew)
	d := newLoginTestDaemon(t)
	loginAsPriorAccount(t, d, loginTestAcctOld)
	d.reconnectBackoff = 20 * time.Millisecond

	var dials atomic.Int32
	d.dial = func(string, *RefreshLoop, func(NATSEvent)) (*nats.Conn, error) {
		if dials.Add(1) == 1 {
			return nil, errors.New("nats down during login")
		}
		return nats.Connect(url)
	}

	if _, e := driveLogin(t, d, cliproto.LoginRequest{URL: srv.URL, APIKey: "ppz_oauth_test"}); e != nil {
		t.Fatalf("login returned error: %v", e)
	}

	if !waitNCConnected(d, 3*time.Second) {
		t.Fatalf("dial failed during login (%d attempts) and nothing kicked the background "+
			"reconnect loop: daemon sits disconnected — and post-clear, `ppz who` sits empty — "+
			"until the next JWT refresh", dials.Load())
	}
}
