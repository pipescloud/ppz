package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// callSend drives handleSend over an in-memory net.Pipe and returns the
// decoded error (nil on success). Mirrors callComplete /
// callSourceDestroy. Used to prove the IPC trust-boundary priority check
// is actually wired into handleSend — the CLI guard (send_priority_test.go)
// rejects bad values before IPC, so without this a deletion of the daemon
// check would pass every other test.
func callSend(t *testing.T, d *Daemon, req cliproto.SendRequest) *cliproto.Error {
	t.Helper()
	params, _ := json.Marshal(req)
	srvConn, cliConn := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer srvConn.Close()
		d.handleSend(context.Background(), srvConn, params)
		close(done)
	}()

	var resp ipcResponse
	if err := json.NewDecoder(cliConn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	cliConn.Close()
	<-done
	return resp.Error
}

// A raw IPC client (no CLI validation in front of it) sending an out-of-
// range priority must be rejected at the daemon trust boundary with
// E_INVALID_PRIORITY — before resolveSendTarget, so no NATS is needed. If
// the `validSendPriority` guard in handleSend were deleted, the request
// would instead fall through to source resolution and fail with a
// different code (or none), which this test catches.
func TestHandleSend_RejectsOutOfRangePriority(t *testing.T) {
	d := newDaemonWithFakeServer(t, http.NewServeMux())

	for _, bad := range []int{-1, -5, 4, 7, 99} {
		req := cliproto.SendRequest{
			Handle:   "someone",
			Channel:  "inbox",
			Payload:  "should never publish",
			Session:  "s1",
			Priority: bad,
		}
		ipcErr := callSend(t, d, req)
		if ipcErr == nil {
			t.Fatalf("priority %d accepted; want E_INVALID_PRIORITY rejection", bad)
		}
		if ipcErr.Code != cliproto.EInvalidPriority {
			t.Fatalf("priority %d rejected with %s, want E_INVALID_PRIORITY", bad, ipcErr.Code)
		}
	}
}
