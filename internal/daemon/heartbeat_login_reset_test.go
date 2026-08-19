package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
)

const (
	loginTestAcctOld = "00000000-0000-0000-0000-0000000000a1"
	loginTestAcctNew = "00000000-0000-0000-0000-0000000000b2"
)

// loginAsPriorAccount seeds the daemon with an existing login for
// accountID, standing in for "the account/connection the daemon was on
// before this test's ppz login".
func loginAsPriorAccount(t *testing.T, d *Daemon, accountID string) {
	t.Helper()
	creds := Credentials{URL: "http://127.0.0.1:1", APIKey: "ppz_prior", AccountID: accountID}
	if err := d.State.SetLogin(creds, accountID, "alpha", "ppz_pri"); err != nil {
		t.Fatalf("SetLogin: %v", err)
	}
}

// TestHandleLogin_ClearsHeartbeatCache — login into a DIFFERENT account
// is an account-boundary crossing: rows stamped before it belong to the
// previous account. handleLogin swaps the NATS connection (so no new
// beats arrive for those handles) but historically left d.Heartbeats
// untouched, so `ppz who` kept showing the old account's agents,
// decaying online -> stale -> offline until a manual daemon restart.
// The field bug: after `ppz login` into a new org, `ppz who` listed
// five runners from the previous org as stale|idle.
//
// The cache must come back empty from a successful cross-account login;
// the next beat round (<= one heartbeat interval) repopulates it,
// identical to the documented daemon-restart behaviour in HeartbeatCache.
func TestHandleLogin_ClearsHeartbeatCache(t *testing.T) {
	srv := okExchangeServer(t, loginTestAcctNew)
	d := newLoginTestDaemon(t)
	loginAsPriorAccount(t, d, loginTestAcctOld)

	// Beats from the pre-login account, as handleSend/subscribeOrgHeartbeats
	// would have stamped them.
	d.Heartbeats.Stamp("alice", loginTestAcctOld, `{"harness":"claude"}`, time.Now())
	d.Heartbeats.Stamp("bob", loginTestAcctOld, `{"harness":"claude"}`, time.Now())

	if _, e := driveLogin(t, d, cliproto.LoginRequest{URL: srv.URL, APIKey: "ppz_oauth_test"}); e != nil {
		t.Fatalf("login returned error: %v", e)
	}

	if got := d.Heartbeats.Snapshot(); len(got) != 0 {
		handles := make([]string, 0, len(got))
		for _, e := range got {
			handles = append(handles, e.Handle)
		}
		t.Fatalf("heartbeat cache still holds %v after cross-account login, want empty: "+
			"pre-login entries belong to the previous account and render as stale ghosts in `ppz who`",
			handles)
	}
}

// TestHandleLogin_FailedLoginKeepsHeartbeatCache — the clear must be
// gated on a successful exchange. A typo'd key or unreachable server
// leaves the daemon exactly as it was, cache included: the operator
// answer to "who was running before my login attempt failed" should
// not change.
func TestHandleLogin_FailedLoginKeepsHeartbeatCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	d := newLoginTestDaemon(t)
	d.Heartbeats.Stamp("alice", loginTestAcctOld, `{"harness":"claude"}`, time.Now())

	if _, e := driveLogin(t, d, cliproto.LoginRequest{URL: srv.URL, APIKey: "ppz_bad_key"}); e == nil {
		t.Fatalf("expected login to fail with E_INVALID_API_KEY, got success")
	}

	if got := d.Heartbeats.Snapshot(); len(got) != 1 || got[0].Handle != "alice" {
		t.Fatalf("failed login must not touch the heartbeat cache, got %v", got)
	}
}
