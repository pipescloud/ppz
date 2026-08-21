//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
)

func TestForwardStdinWritesPayloadVerbatim(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ppz-forward-stdin-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"no newline", "hello"},
		{"escape sequence", "\x1b[13u"},
		{"already has newline", "hello\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(sock)
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			t.Cleanup(func() { _ = ln.Close() })

			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var req struct {
					Method string `json:"method"`
				}
				_ = json.NewDecoder(conn).Decode(&req)
				_ = json.NewEncoder(conn).Encode(cliproto.ReadEvent{
					Message: &cliproto.ReadMessage{Payload: tc.payload},
				})
			}()

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			defer r.Close()
			defer w.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			// No lease held (fresh leaseState → "" holder), so enforcement
			// is inert and every message forwards verbatim, as before.
			go forwardStdin(ctx, "myhost", w, newLeaseState())

			buf := make([]byte, 64)
			_ = r.SetReadDeadline(time.Now().Add(time.Second))
			n, _ := r.Read(buf)
			got := string(buf[:n])

			if got != tc.payload {
				t.Errorf("forwardStdin wrote %q, want %q (verbatim — no appended newline)", got, tc.payload)
			}
		})
	}
}

// TestForwardStdinRequestIsolatesHostCursor pins the two properties of the
// pty host's .stdin follow that its once-only delivery depends on, neither
// of which is visible from the bytes that reach the PTY.
//
// Session: the follow is cursor-advancing, so its session string is the
// namespace of the watermark that decides what a resumed share re-reads. It
// must NOT be the wrapped agent's own session. terminalShareEnv exports
// PPZ_SESSION=<handle> into the child (terminal.go), so `ppz read` /
// `ppz subs read` run BY the agent send Session: sessionID() == <handle>
// with advance enabled. Sharing that namespace lets an agent that reads its
// own .stdin (directly, or via a subscription matching it) advance the
// host's watermark and silently consume commands the host then never
// delivers. NoAdvance made the host immune to that before; a dedicated
// namespace is what keeps it immune now.
//
// Sender: cursor-advancing reads auto-emit ack:read (daemon read.go), and
// the daemon resolves the ack's sender as senderForRequest(req.Sender,
// State.Current(req.Session)). State.Current is empty for a pty source, so
// without an explicit Sender the ack ships attributed to nobody — or worse,
// to whatever the agent last ran `ppz set handle` with. tui.go's inbox
// follow already sets Sender for this reason.
func TestForwardStdinRequestIsolatesHostCursor(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ppz-forward-stdin-req-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	// The agent inside the pty runs with PPZ_SESSION=<handle>; sessionID()
	// therefore returns the handle for anything it invokes.
	const handle = "myhost"
	t.Setenv("PPZ_SESSION", handle)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	gotReq := make(chan cliproto.ReadRequest, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		var env struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.NewDecoder(conn).Decode(&env) != nil {
			return
		}
		var req cliproto.ReadRequest
		_ = json.Unmarshal(env.Params, &req)
		gotReq <- req
	}()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go forwardStdin(ctx, handle, w, newLeaseState())

	var req cliproto.ReadRequest
	select {
	case req = <-gotReq:
	case <-time.After(2 * time.Second):
		t.Fatal("forwardStdin never sent a read request")
	}

	if req.NoAdvance {
		t.Error("follow is NoAdvance — the cursor never moves, so a resumed share re-drains the retained window")
	}
	if req.Session == "" {
		t.Error("follow has no Session — the watermark lands in the daemon's default namespace, shared with every other default-session reader")
	}
	if req.Session == sessionID() {
		t.Errorf("follow shares session %q with the wrapped agent: an agent reading its own .stdin advances the host's watermark and eats queued commands", req.Session)
	}
	if req.Sender != handle {
		t.Errorf("follow Sender = %q, want %q — ack:read for .stdin is attributed to whatever State.Current resolves to instead of the agent", req.Sender, handle)
	}
}
