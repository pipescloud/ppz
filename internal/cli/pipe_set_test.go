package cli

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// `ppz pipe set [HANDLE.]NAME [--ttl --max-msgs --max-bytes]` changes the
// retention of an existing pipe. Target grammar is identical to `pipe
// create` / `pipe destroy` (Phase 1.5.1 rule: a bare LEAF addresses an
// uncollared pipe at the session's current namespace regardless of the
// current handle; HANDLE.NAME is the explicit collared form), and the
// flag names are identical to `pipe create` so there is one retention
// vocabulary rather than two.

func TestCmdPipeSet_BareNameSkipsCurrentHandle(t *testing.T) {
	t.Setenv("PPZ_SESSION", "pipe-set-test")
	t.Setenv("PPZ_CURRENT_HANDLE", "env-current")
	sock := pipeSetTestSocket(t)
	requests := servePipeSetDaemon(t, sock, "daemon-current")

	if err := cmdPipeSet([]string{"alerts", "--max-msgs=50"}); err != nil {
		t.Fatalf("cmdPipeSet: %v", err)
	}

	if requests.sets.count() != 1 {
		t.Fatalf("pipe set request count = %d, want 1", requests.sets.count())
	}
	got := requests.sets.at(0)
	if got.Handle != "" || got.Name != "alerts" {
		t.Fatalf("pipe set resolved to handle=%q name=%q, want empty handle + name=alerts (bare LEAF is uncollared)", got.Handle, got.Name)
	}
}

func TestCmdPipeSet_DottedTargetIsCollared(t *testing.T) {
	t.Setenv("PPZ_SESSION", "pipe-set-test")
	sock := pipeSetTestSocket(t)
	requests := servePipeSetDaemon(t, sock, "daemon-current")

	if err := cmdPipeSet([]string{"chat.archive", "--ttl=168h"}); err != nil {
		t.Fatalf("cmdPipeSet: %v", err)
	}

	got := requests.sets.at(0)
	if got.Handle != "chat" || got.Name != "archive" {
		t.Fatalf("pipe set resolved to handle=%q name=%q, want chat/archive", got.Handle, got.Name)
	}
}

// Only the flags the user actually typed travel on the wire — the others
// stay nil so the server preserves what's stored. This is the CLI half of
// the merge contract.
func TestCmdPipeSet_OnlyTypedFlagsAreSent(t *testing.T) {
	t.Setenv("PPZ_SESSION", "pipe-set-test")
	sock := pipeSetTestSocket(t)
	requests := servePipeSetDaemon(t, sock, "daemon-current")

	if err := cmdPipeSet([]string{"chat.archive", "--max-msgs=50"}); err != nil {
		t.Fatalf("cmdPipeSet: %v", err)
	}

	got := requests.sets.at(0)
	if got.MaxMsgs == nil || *got.MaxMsgs != 50 {
		t.Errorf("MaxMsgs = %v, want 50", got.MaxMsgs)
	}
	if got.TTLSeconds != nil {
		t.Errorf("TTLSeconds = %v, want nil (not typed — server must preserve the stored value)", *got.TTLSeconds)
	}
	if got.MaxBytes != nil {
		t.Errorf("MaxBytes = %v, want nil (not typed)", *got.MaxBytes)
	}
}

// --ttl / --max-bytes parse exactly as they do on `pipe create`:
// Go duration strings, and int-or-suffixed sizes.
func TestCmdPipeSet_ParsesDurationAndSizeFlags(t *testing.T) {
	t.Setenv("PPZ_SESSION", "pipe-set-test")
	sock := pipeSetTestSocket(t)
	requests := servePipeSetDaemon(t, sock, "daemon-current")

	if err := cmdPipeSet([]string{"chat.archive", "--ttl=168h", "--max-bytes=64MiB"}); err != nil {
		t.Fatalf("cmdPipeSet: %v", err)
	}

	got := requests.sets.at(0)
	if got.TTLSeconds == nil || *got.TTLSeconds != 604800 {
		t.Errorf("TTLSeconds = %v, want 604800 (168h)", got.TTLSeconds)
	}
	if got.MaxBytes == nil || *got.MaxBytes != 64*1024*1024 {
		t.Errorf("MaxBytes = %v, want 67108864 (64MiB)", got.MaxBytes)
	}
}

// Flags may precede the target, same as `pipe create` (splitTargetAndFlags
// handles the interleaving).
func TestCmdPipeSet_FlagsBeforeTarget(t *testing.T) {
	t.Setenv("PPZ_SESSION", "pipe-set-test")
	sock := pipeSetTestSocket(t)
	requests := servePipeSetDaemon(t, sock, "daemon-current")

	if err := cmdPipeSet([]string{"--max-msgs=50", "chat.archive"}); err != nil {
		t.Fatalf("cmdPipeSet: %v", err)
	}
	if got := requests.sets.at(0); got.Handle != "chat" || got.Name != "archive" {
		t.Fatalf("pipe set resolved to handle=%q name=%q, want chat/archive", got.Handle, got.Name)
	}
}

// `ppz pipe set NAME` with no flags names no change. Erroring beats
// round-tripping to the server to be told nothing happened.
func TestCmdPipeSet_NoFlagsIsAnError(t *testing.T) {
	t.Setenv("PPZ_SESSION", "pipe-set-test")
	sock := pipeSetTestSocket(t)
	requests := servePipeSetDaemon(t, sock, "daemon-current")

	err := cmdPipeSet([]string{"chat.archive"})
	if err == nil {
		t.Fatal("cmdPipeSet with no retention flags: got nil error, want a failure")
	}
	if requests.sets.count() != 0 {
		t.Errorf("no-flag `pipe set` reached the daemon %d times, want 0", requests.sets.count())
	}
}

// `ppz pipe <tab>` and `ppz pipe` with no subcommand must both know about
// `set`, or the verb is invisible to users and shells.
func TestPipeGroup_DispatchesSet(t *testing.T) {
	t.Setenv("PPZ_SESSION", "pipe-set-test")
	sock := pipeSetTestSocket(t)
	requests := servePipeSetDaemon(t, sock, "daemon-current")

	if err := cmdPipeGroup([]string{"set", "chat.archive", "--max-msgs=50"}); err != nil {
		t.Fatalf("cmdPipeGroup set: %v", err)
	}
	if requests.sets.count() != 1 {
		t.Errorf("`ppz pipe set` did not dispatch to cmdPipeSet (got %d requests)", requests.sets.count())
	}
}

func pipeSetTestSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ppz-pipe-set-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)
	return sock
}

type pipeSetRequests struct {
	sets recorder[cliproto.PipeSetRequest]
}

func servePipeSetDaemon(t *testing.T, sock, current string) *pipeSetRequests {
	t.Helper()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen fake daemon: %v", err)
	}
	requests := &pipeSetRequests{}
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
			var req struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				_ = conn.Close()
				continue
			}
			switch req.Method {
			case cliproto.IPCStatus:
				_ = json.NewEncoder(conn).Encode(map[string]any{
					"result": cliproto.StatusReply{DaemonPID: 1234, LoggedIn: true, Current: current},
				})
			case cliproto.IPCPipeSet:
				var ps cliproto.PipeSetRequest
				_ = json.Unmarshal(req.Params, &ps)
				requests.sets.add(ps)
				_ = json.NewEncoder(conn).Encode(map[string]any{
					"result": cliproto.PipeSetReply{Handle: ps.Handle, Name: ps.Name},
				})
			}
			_ = conn.Close()
		}
	}()

	return requests
}
