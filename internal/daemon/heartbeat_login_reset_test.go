package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// TestHandleLogin_ClearsHeartbeatCache — login is an account-boundary
// crossing: any heartbeat rows stamped before it belong to whatever
// account/connection the daemon was on previously. handleLogin swaps
// the NATS connection (so no new beats arrive for those handles) but
// historically left d.Heartbeats untouched, so `ppz who` kept showing
// the old account's agents, decaying online -> stale -> offline until
// a manual daemon restart. The field bug: after `ppz login` into a new
// org, `ppz who` listed five runners from the previous org as
// stale|idle.
//
// The cache must come back empty from a successful login; the next
// beat round (<= one heartbeat interval) repopulates it, identical to
// the documented daemon-restart behaviour in HeartbeatCache.
func TestHandleLogin_ClearsHeartbeatCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cliproto.AuthExchangeReply{
			NATSURL:      "nats://127.0.0.1:1", // fails fast (DNS) → connect is best-effort
			AccountID:    "00000000-0000-0000-0000-0000000000b2",
			AccountName:  "beta",
			NATSUserJWT:  "jwt",
			NATSUserSeed: "seed",
			ExpiresAt:    time.Now().Add(5 * time.Minute),
		})
	}))
	defer srv.Close()

	d := &Daemon{
		State:      NewState(t.TempDir()),
		NATSEvents: newNATSEventRing(natsEventRingCap),
		Follows:    newFollowRegistry(),
		Watches:    newWatchRegistry(),
		Heartbeats: NewHeartbeatCache(),
		HTTP:       &http.Client{Timeout: 2 * time.Second},
	}
	t.Cleanup(func() {
		if d.Refresh != nil {
			d.Refresh.Stop()
		}
		if d.NC != nil {
			d.NC.Close()
		}
	})

	// Beats from the pre-login account, as handleSend/subscribeOrgHeartbeats
	// would have stamped them.
	d.Heartbeats.Stamp("alice", `{"harness":"claude"}`, time.Now())
	d.Heartbeats.Stamp("bob", `{"harness":"claude"}`, time.Now())

	srvConn, cliConn := net.Pipe()
	params, _ := json.Marshal(cliproto.LoginRequest{URL: srv.URL, APIKey: "ppz_oauth_test"})
	go func() {
		d.handleLogin(context.Background(), srvConn, params)
		srvConn.Close()
	}()

	// Drain the IPC reply so handleLogin's writeIPC doesn't block on the
	// unbuffered pipe.
	_ = cliConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(cliConn).ReadBytes('\n')
	if err != nil && err != io.EOF {
		t.Fatalf("reading IPC reply: %v", err)
	}
	var resp struct {
		Result cliproto.LoginReply `json:"result"`
		Error  *cliproto.Error     `json:"error"`
	}
	_ = json.Unmarshal(line, &resp)
	if resp.Error != nil {
		t.Fatalf("login returned error: %v", resp.Error)
	}

	if got := d.Heartbeats.Snapshot(); len(got) != 0 {
		handles := make([]string, 0, len(got))
		for _, e := range got {
			handles = append(handles, e.Handle)
		}
		t.Fatalf("heartbeat cache still holds %v after login, want empty: "+
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

	d := &Daemon{
		State:      NewState(t.TempDir()),
		NATSEvents: newNATSEventRing(natsEventRingCap),
		Follows:    newFollowRegistry(),
		Watches:    newWatchRegistry(),
		Heartbeats: NewHeartbeatCache(),
		HTTP:       &http.Client{Timeout: 2 * time.Second},
	}

	d.Heartbeats.Stamp("alice", `{"harness":"claude"}`, time.Now())

	srvConn, cliConn := net.Pipe()
	params, _ := json.Marshal(cliproto.LoginRequest{URL: srv.URL, APIKey: "ppz_bad_key"})
	go func() {
		d.handleLogin(context.Background(), srvConn, params)
		srvConn.Close()
	}()

	_ = cliConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(cliConn).ReadBytes('\n')
	if err != nil && err != io.EOF {
		t.Fatalf("reading IPC reply: %v", err)
	}
	var resp struct {
		Error *cliproto.Error `json:"error"`
	}
	_ = json.Unmarshal(line, &resp)
	if resp.Error == nil {
		t.Fatalf("expected login to fail with E_INVALID_API_KEY, got success")
	}

	if got := d.Heartbeats.Snapshot(); len(got) != 1 || got[0].Handle != "alice" {
		t.Fatalf("failed login must not touch the heartbeat cache, got %v", got)
	}
}
