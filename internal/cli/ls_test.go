package cli

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// `ppz ls clancy%` (pattern argument, no --watch) should filter the
// snapshot table to matching pipes rather than rejecting the args.
// Without this, the natural `ls` invocation errors out and the user
// has to either add --watch (changes semantics from "snapshot now" to
// "block until unread arrives") or pipe through grep (loses column
// alignment and the daemon's payload truncation).
//
// Contract: cmdLs forwards positional args as ListRequest.Patterns
// over IPCList. The daemon side already has buildFilteredList wired
// for ListWatchRequest; this opens the door from plain ls.
func TestCmdLs_PatternsForwardToList(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ppz-ls-pat-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	requests := serveLsListDaemon(t, sock)

	if err := cmdLs([]string{"clancy%"}); err != nil {
		t.Fatalf("cmdLs with pattern arg: %v (want filtered list, not error)", err)
	}

	if requests.lists.count() != 1 {
		t.Fatalf("IPCList request count = %d, want 1", requests.lists.count())
	}
	got := requests.lists.at(0)
	if len(got.Patterns) != 1 || got.Patterns[0] != "clancy%" {
		t.Fatalf("ListRequest.Patterns = %v, want [clancy%%]", got.Patterns)
	}
}

// Bare `ppz ls` (no args) must keep working unchanged — every other
// IPCList caller (source.go, pipe.go, completion.go)
// passes no patterns and expects the full snapshot.
func TestCmdLs_NoArgsSendsEmptyPatterns(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ppz-ls-nopat-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	requests := serveLsListDaemon(t, sock)

	if err := cmdLs(nil); err != nil {
		t.Fatalf("cmdLs no args: %v", err)
	}
	if requests.lists.count() != 1 {
		t.Fatalf("IPCList request count = %d, want 1", requests.lists.count())
	}
	if len(requests.lists.at(0).Patterns) != 0 {
		t.Fatalf("ListRequest.Patterns = %v, want empty", requests.lists.at(0).Patterns)
	}
}

// `ppz ls "*.heartbeat" --json` must honor --json even though the flag
// follows the positional glob. Go's flag package stops parsing at the
// first non-flag arg, so without pre-separating flags the --json token
// was silently swallowed into ListRequest.Patterns (becoming a bogus
// pattern) and the flag never took effect — the reported bug where
// `ls --json` emitted JSON but `ls "*.heartbeat" --json` did not.
func TestCmdLs_FlagAfterPattern(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ppz-ls-flagpos-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	requests := serveLsListDaemon(t, sock)

	if err := cmdLs([]string{"*.heartbeat", "--json"}); err != nil {
		t.Fatalf("cmdLs pattern then flag: %v", err)
	}

	if requests.lists.count() != 1 {
		t.Fatalf("IPCList request count = %d, want 1", requests.lists.count())
	}
	// The flag must not leak into the pattern list: only the glob is a
	// pattern. If --json is present here, flag parsing stopped early.
	got := requests.lists.at(0)
	if len(got.Patterns) != 1 || got.Patterns[0] != "*.heartbeat" {
		t.Fatalf("ListRequest.Patterns = %v, want [*.heartbeat] (flag leaked into patterns?)", got.Patterns)
	}
}

// lsDaemonRequests records both verbs `ppz ls` can issue: the snapshot
// (IPCList) and, under --watch, the blocking form (IPCListWatch). They
// carry the same flags and must not drift — a flag set on one and
// forgotten on the other renders headers with no data behind them.
type lsDaemonRequests struct {
	lists   recorder[cliproto.ListRequest]
	watches recorder[cliproto.ListWatchRequest]
}

func serveLsListDaemon(t *testing.T, sock string) *lsDaemonRequests {
	t.Helper()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen fake daemon: %v", err)
	}
	requests := &lsDaemonRequests{}
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
			case cliproto.IPCList:
				var lr cliproto.ListRequest
				_ = json.Unmarshal(req.Params, &lr)
				requests.lists.add(lr)
				_ = json.NewEncoder(conn).Encode(map[string]any{
					"result": cliproto.ListReply{},
				})
			case cliproto.IPCListWatch:
				var lw cliproto.ListWatchRequest
				_ = json.Unmarshal(req.Params, &lw)
				requests.watches.add(lw)
				_ = json.NewEncoder(conn).Encode(map[string]any{
					"result": cliproto.ListReply{},
				})
			}
			_ = conn.Close()
		}
	}()

	return requests
}
