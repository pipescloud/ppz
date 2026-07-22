package cli

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pipescloud/ppz/internal/cliproto"
)

func TestCmdTerminalShare_BareInvocationPrefersEnvCurrentHandle(t *testing.T) {
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true command not available")
	}
	t.Setenv("PPZ_SESSION", "terminal-current-test")
	t.Setenv("PPZ_CURRENT_HANDLE", "env-current")

	dir, err := os.MkdirTemp("/tmp", "ppz-terminal-current-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	requests := serveTerminalCurrentDaemon(t, sock, "daemon-current")

	if err := cmdTerminalShare([]string{"--", truePath}); err != nil {
		t.Fatalf("cmdTerminalShare bare: %v", err)
	}

	// Bare share upgrades the current source to a full pty terminal via a
	// single IPCEnsurePTY call. It must target the env-provided current
	// handle, not the daemon's own current.
	if requests.ensurePTY.count() != 1 {
		t.Fatalf("ensure-pty request count = %d, want 1", requests.ensurePTY.count())
	}
	for _, got := range requests.ensurePTY.snapshot() {
		if got.Handle != "env-current" {
			t.Fatalf("terminal share upgraded handle %q, want env-current", got.Handle)
		}
	}
}

type terminalCurrentRequests struct {
	ensurePTY recorder[cliproto.EnsurePTYRequest]
}

func serveTerminalCurrentDaemon(t *testing.T, sock, current string) *terminalCurrentRequests {
	t.Helper()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen fake daemon: %v", err)
	}
	requests := &terminalCurrentRequests{}
	done := make(chan struct{})
	t.Cleanup(func() { <-done })
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(sock)
	})

	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleTerminalCurrentDaemonConn(conn, current, requests)
		}
	}()

	return requests
}

func handleTerminalCurrentDaemonConn(conn net.Conn, current string, requests *terminalCurrentRequests) {
	defer conn.Close()
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}

	enc := json.NewEncoder(conn)
	switch req.Method {
	case cliproto.IPCStatus:
		_ = enc.Encode(map[string]any{
			"result": cliproto.StatusReply{DaemonPID: 1234, LoggedIn: true, Current: current},
		})
	case cliproto.IPCEnsurePTY:
		var ep cliproto.EnsurePTYRequest
		_ = json.Unmarshal(req.Params, &ep)
		requests.ensurePTY.add(ep)
		_ = enc.Encode(map[string]any{
			"result": cliproto.EnsurePTYReply{Handle: ep.Handle, Kind: "pty"},
		})
	case cliproto.IPCSend:
		var br cliproto.SendRequest
		_ = json.Unmarshal(req.Params, &br)
		_ = enc.Encode(map[string]any{
			"result": cliproto.SendReply{
				ID:      "test-id",
				Subject: "test." + br.Handle + "." + br.Channel,
				Bytes:   len(br.Payload),
			},
		})
	case cliproto.IPCRead:
		_, _ = io.Copy(io.Discard, conn)
	}
}
