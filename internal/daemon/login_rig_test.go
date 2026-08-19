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

// Shared drive rig for handleLogin tests. Three tests used to carry
// byte-identical copies of the exchange server + Daemon literal +
// net.Pipe IPC drain; new login tests should compose these helpers
// instead.

// okExchangeReply is the canonical successful /auth/exchange body for
// accountID. NATSURL points at a closed local port so the best-effort
// NATS connect fails fast (DNS-free) unless a test overrides dial.
func okExchangeReply(accountID string) cliproto.AuthExchangeReply {
	return cliproto.AuthExchangeReply{
		NATSURL:      "nats://127.0.0.1:1",
		AccountID:    accountID,
		AccountName:  "beta",
		NATSUserJWT:  "jwt",
		NATSUserSeed: "seed",
		ExpiresAt:    time.Now().Add(5 * time.Minute),
	}
}

// okExchangeServer serves a successful /auth/exchange minting into
// accountID, for tests that don't need to inspect the request.
func okExchangeServer(t *testing.T, accountID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okExchangeReply(accountID))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newLoginTestDaemon builds the minimal Daemon that handleLogin needs,
// with refresh-loop / NC teardown registered.
func newLoginTestDaemon(t *testing.T) *Daemon {
	t.Helper()
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
	return d
}

// driveLogin runs handleLogin over a net.Pipe and returns the decoded
// IPC reply. Fails the test on transport/decode problems (a truncated
// or empty reply must not silently satisfy an error==nil assertion).
func driveLogin(t *testing.T, d *Daemon, req cliproto.LoginRequest) (cliproto.LoginReply, *cliproto.Error) {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	params, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshalling LoginRequest: %v", err)
	}
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
	if len(line) == 0 {
		t.Fatalf("handleLogin wrote no IPC reply")
	}
	var resp struct {
		Result cliproto.LoginReply `json:"result"`
		Error  *cliproto.Error     `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decoding IPC reply %q: %v", line, err)
	}
	return resp.Result, resp.Error
}
