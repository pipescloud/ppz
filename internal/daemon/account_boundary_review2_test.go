package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// RED tests for the PR #193 round-2 review. The heartbeat findings
// (1, 4, 6) share one contract: `ppz who` must only render beats
// received under the CURRENT account, no matter when a subscription
// callback lands relative to a login/logout — Close doesn't join
// in-flight callbacks, and no lock spans the login sequence, so
// ordering alone can never be airtight.

// presenceMsg builds the NATS message a presence subscription would
// deliver, carrying an envelope.
//
// ACL Phase 0b moved presence off the <account>.> firehose onto its own
// <account>._presence.<handle> family — the subject grammar changed, the
// account-boundary invariant these tests pin did not.
func presenceMsg(account, handle, payload string) *nats.Msg {
	data, _ := json.Marshal(envelope.Message{Payload: payload})
	return &nats.Msg{
		Subject: natsubj.PresenceSubject(uuid.MustParse(account), "", handle),
		Data:    data,
	}
}

// driveWho runs handleWho over a net.Pipe and returns the entries.
func driveWho(t *testing.T, d *Daemon) []cliproto.WhoEntry {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	go func() {
		d.handleWho(context.Background(), srvConn, json.RawMessage(`{}`))
		srvConn.Close()
	}()
	_ = cliConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(cliConn).ReadBytes('\n')
	if err != nil && err != io.EOF {
		t.Fatalf("reading IPC reply: %v", err)
	}
	if len(line) == 0 {
		t.Fatalf("handleWho wrote no IPC reply")
	}
	var resp struct {
		Result cliproto.WhoReply `json:"result"`
		Error  *cliproto.Error   `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decoding IPC reply %q: %v", line, err)
	}
	if resp.Error != nil {
		t.Fatalf("handleWho returned error: %v", resp.Error)
	}
	return resp.Result.Entries
}

// TestWho_LateOldOrgCallbackAfterLogin — findings 1+4: an old-org
// subscription callback that finishes executing AFTER the login
// sequence (Close doesn't join in-flight callbacks; swapNC ordering
// only narrows the window) must not resurrect the old account's agent
// in `ppz who` under the new account.
func TestWho_LateOldOrgCallbackAfterLogin(t *testing.T) {
	srv := okExchangeServer(t, loginTestAcctNew)
	d := newLoginTestDaemon(t)
	loginAsPriorAccount(t, d, loginTestAcctOld)

	if _, e := driveLogin(t, d, cliproto.LoginRequest{URL: srv.URL, APIKey: "ppz_oauth_test"}); e != nil {
		t.Fatalf("login returned error: %v", e)
	}

	// The straggler: an old-org callback completing after login is done.
	d.stampPresence(uuid.MustParse(loginTestAcctOld), presenceMsg(loginTestAcctOld, "alice", `{"harness":"claude"}`))

	if got := driveWho(t, d); len(got) != 0 {
		t.Fatalf("old-org beat delivered after login renders in `ppz who` under the new "+
			"account (%v): rows must be scoped to the account they arrived under", got)
	}
}

// TestWho_NewOrgBeatDuringLoginSurvives — finding 6: a NEW-org beat
// arriving mid-login (a concurrent kickConnect rebuild can arm the new
// org's subscription before the login sequence finishes) must not be
// erased by the account-boundary clear — that under-reports `ppz who`
// for up to a full heartbeat interval.
func TestWho_NewOrgBeatDuringLoginSurvives(t *testing.T) {
	srv := okExchangeServer(t, loginTestAcctNew)
	d := newLoginTestDaemon(t)
	loginAsPriorAccount(t, d, loginTestAcctOld)
	d.reconnectBackoff = time.Hour // park the dial-failure recovery loop
	// Only the FIRST dial is the mid-login window; the recovery loop's
	// immediate first retry re-enters this stub and must not re-stamp,
	// or the test passes for the wrong reason (a post-clear re-stamp
	// instead of the clear sparing the row).
	var dials atomic.Int32
	d.dial = func(string, *RefreshLoop, func(NATSEvent)) (*nats.Conn, error) {
		if dials.Add(1) == 1 {
			// Mid-login: the new org's subscription (armed by a
			// concurrent rebuild) delivers a beat.
			d.stampPresence(uuid.MustParse(loginTestAcctNew), presenceMsg(loginTestAcctNew, "bob", `{"harness":"claude"}`))
		}
		return nil, os.ErrDeadlineExceeded
	}

	if _, e := driveLogin(t, d, cliproto.LoginRequest{URL: srv.URL, APIKey: "ppz_oauth_test"}); e != nil {
		t.Fatalf("login returned error: %v", e)
	}

	got := driveWho(t, d)
	if len(got) != 1 || got[0].Handle != "bob" {
		t.Fatalf("new-org beat delivered during login must survive the account-boundary "+
			"clear and render in `ppz who`, got %v", got)
	}
}

// TestWho_LogoutHidesRows — finding 1 (logout side): once the daemon is
// logged out, `ppz who` must render nothing — including rows stamped
// while logged in AND a straggler callback landing after the logout
// wipe (the watcher's creds fileSig never changes again, so a
// post-clear stamp would otherwise persist until daemon restart).
func TestWho_LogoutHidesRows(t *testing.T) {
	d := newLoginTestDaemon(t)
	loginAsPriorAccount(t, d, loginTestAcctOld)
	d.stampPresence(uuid.MustParse(loginTestAcctOld), presenceMsg(loginTestAcctOld, "alice", `{"harness":"claude"}`))

	// Logout: credentials file removed, state reloaded (what watchState
	// does on the creds-gone transition).
	if err := os.Remove(filepath.Join(d.State.Home(), fileCredentials)); err != nil {
		t.Fatalf("removing credentials file: %v", err)
	}
	if err := d.State.LoadFromDisk(); err != nil {
		t.Fatalf("LoadFromDisk: %v", err)
	}
	// The straggler, finishing after the logout wipe.
	d.stampPresence(uuid.MustParse(loginTestAcctOld), presenceMsg(loginTestAcctOld, "eve", `{"harness":"claude"}`))

	if got := driveWho(t, d); len(got) != 0 {
		t.Fatalf("logged-out daemon renders %v in `ppz who`: rows must be scoped to the "+
			"current account (none, when logged out)", got)
	}
}

// TestSetLogin_ResetsCompletionCache — finding 2: SetLogin resets
// knownPipes/handleManifold/pipesLoaded but left completionSources/
// completionLoaded — so after a cross-account login every tab press
// serves org A's source and pipe names as fresh (Stale:false) under
// org B, unbounded, because nothing else repopulates the snapshot
// until an ls/send/connect happens to run.
func TestSetLogin_ResetsCompletionCache(t *testing.T) {
	st := NewState(t.TempDir())
	if err := st.SetLogin(Credentials{URL: "http://127.0.0.1:1", APIKey: "k1"}, loginTestAcctOld, "alpha", "k1"); err != nil {
		t.Fatalf("SetLogin: %v", err)
	}
	st.SetCompletionSnapshot([]cliproto.CompleteSource{{Handle: "alice", Pipes: []string{"inbox"}}})

	if err := st.SetLogin(Credentials{URL: "http://127.0.0.1:1", APIKey: "k2"}, loginTestAcctNew, "beta", "k2"); err != nil {
		t.Fatalf("SetLogin: %v", err)
	}
	if got, ok := st.CompletionSnapshot(); ok {
		t.Fatalf("completion cache survived a login (%v): tab completion would serve the "+
			"previous org's vocabulary as fresh", got)
	}
}

// TestLoadFromDisk_ResetsCompletionCache — finding 2's other site:
// reload zeroes every other per-login cache ("status should probe");
// the completion snapshot must follow.
func TestLoadFromDisk_ResetsCompletionCache(t *testing.T) {
	st := NewState(t.TempDir())
	if err := st.SetLogin(Credentials{URL: "http://127.0.0.1:1", APIKey: "k1"}, loginTestAcctOld, "alpha", "k1"); err != nil {
		t.Fatalf("SetLogin: %v", err)
	}
	st.SetCompletionSnapshot([]cliproto.CompleteSource{{Handle: "alice"}})
	if err := st.LoadFromDisk(); err != nil {
		t.Fatalf("LoadFromDisk: %v", err)
	}
	if got, ok := st.CompletionSnapshot(); ok {
		t.Fatalf("completion cache survived LoadFromDisk (%v)", got)
	}
}

// TestSubs_AccountScoped — finding 3: subs are stored as bare
// <handle>.<pipe> under subs/<session>.json with no account in the key
// (unlike Cursors, whose CursorKey embeds accountID). After a
// cross-account login the handlers prepend the NEW account to the OLD
// org's subjects: colliding handle names silently bind the old
// subscription set to new-org data; non-colliding ones render as dead
// rows. The set must be scoped per account — and come back when the
// user logs back into the original account.
func TestSubs_AccountScoped(t *testing.T) {
	d := New(t.TempDir(), "")
	credsA := Credentials{URL: "http://127.0.0.1:1", APIKey: "kA"}
	credsB := Credentials{URL: "http://127.0.0.1:1", APIKey: "kB"}
	if err := d.State.SetLogin(credsA, loginTestAcctOld, "alpha", "kA"); err != nil {
		t.Fatalf("SetLogin A: %v", err)
	}
	if err := d.Subs.Add("sess1", "alice.inbox"); err != nil {
		t.Fatalf("Subs.Add: %v", err)
	}

	if err := d.State.SetLogin(credsB, loginTestAcctNew, "beta", "kB"); err != nil {
		t.Fatalf("SetLogin B: %v", err)
	}
	if got := d.Subs.List("sess1"); len(got) != 0 {
		t.Fatalf("account A's subs leak into account B (%v): the handlers would prepend "+
			"B's account id to A's subjects", got)
	}

	if err := d.State.SetLogin(credsA, loginTestAcctOld, "alpha", "kA"); err != nil {
		t.Fatalf("SetLogin A again: %v", err)
	}
	if got := d.Subs.List("sess1"); len(got) != 1 || got[0] != "alice.inbox" {
		t.Fatalf("account A's subs must survive a round-trip through account B, got %v", got)
	}
}

// TestWatchState_TransientLoadErrorDoesNotWipe — finding 5: a
// transient credentials read failure (EACCES here; EMFILE/EIO in the
// wild) is indistinguishable from logout in the current code — the
// LoadFromDisk error is discarded, creds read as nil, and the watcher
// destructively wipes the cache and drops NATS while the user is
// genuinely logged in. A failed reload must change nothing.
func TestWatchState_TransientLoadErrorDoesNotWipe(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0000 is not an error for root")
	}
	d := newLoginTestDaemon(t)
	loginAsPriorAccount(t, d, loginTestAcctOld)
	d.Heartbeats.Stamp("alice", loginTestAcctOld, `{"harness":"claude"}`, time.Now())

	credPath := filepath.Join(d.State.Home(), fileCredentials)
	if err := os.Chmod(credPath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(credPath, 0o600) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hupCh := make(chan os.Signal, 1)
	go d.watchState(ctx, hupCh)
	hupCh <- syscall.SIGHUP

	// The wipe (when it fires) lands within one reload; hold the
	// observation window a few ticks past that.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(d.Heartbeats.Snapshot()) != 1 {
			t.Fatalf("transient credentials read error wiped the heartbeat cache")
		}
		if _, ok := d.State.Credentials(); !ok {
			t.Fatalf("transient credentials read error zeroed in-memory credentials: " +
				"indistinguishable from logout, daemon drops NATS while genuinely logged in")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestLoadFromDisk_ReadErrorKeepsState — finding 5 at the State layer:
// LoadFromDisk zeroes every field BEFORE reading the files, so a read
// error leaves the state destroyed regardless of what the caller does
// with the returned error. A failed reload must keep the prior state.
func TestLoadFromDisk_ReadErrorKeepsState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0000 is not an error for root")
	}
	st := NewState(t.TempDir())
	if err := st.SetLogin(Credentials{URL: "http://127.0.0.1:1", APIKey: "k1"}, loginTestAcctOld, "alpha", "k1"); err != nil {
		t.Fatalf("SetLogin: %v", err)
	}
	credPath := filepath.Join(st.Home(), fileCredentials)
	if err := os.Chmod(credPath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(credPath, 0o600) })

	if err := st.LoadFromDisk(); err == nil {
		t.Fatalf("LoadFromDisk succeeded reading an unreadable credentials file")
	}
	if _, ok := st.Credentials(); !ok {
		t.Fatalf("LoadFromDisk error destroyed in-memory credentials — reload must be " +
			"all-or-nothing")
	}
	if got := st.AccountID(); got != loginTestAcctOld {
		t.Fatalf("LoadFromDisk error destroyed accountID, got %q", got)
	}
}

// TestHandleLogin_EmptyAccountIDFails — finding 7: the exchange decode
// validates nothing. An empty account_id passes the boundary-clear gate
// (priorAccount != ""), wipes the cache, then fails uuid.Parse so the
// org subscription never re-arms — and no recovery path re-exchanges
// (bootstrapNATS gates on NATSURL emptiness, rebuildNC on the same
// failing parse). Reject it at the decode and leave the daemon
// untouched.
func TestHandleLogin_EmptyAccountIDFails(t *testing.T) {
	srv := okExchangeServer(t, "")
	d := newLoginTestDaemon(t)
	loginAsPriorAccount(t, d, loginTestAcctOld)
	d.Heartbeats.Stamp("alice", loginTestAcctOld, `{"harness":"claude"}`, time.Now())

	if _, e := driveLogin(t, d, cliproto.LoginRequest{URL: srv.URL, APIKey: "ppz_oauth_test"}); e == nil {
		t.Fatalf("login with an empty exchange account_id must fail — it can never arm " +
			"an org subscription and has no recovery path")
	}
	if got := d.Heartbeats.Snapshot(); len(got) != 1 {
		t.Fatalf("failed (empty-account) login must not touch the heartbeat cache, got %v", got)
	}
	if got := d.State.AccountID(); got != loginTestAcctOld {
		t.Fatalf("failed (empty-account) login must not overwrite stored login, got account %q", got)
	}
}
