package cli

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pipescloud/ppz/internal/cliproto"
)

func TestRunRead_BareInboxResolvesToCurrentSourceInbox(t *testing.T) {
	t.Setenv("PPZ_SESSION", "read-inbox-test")
	dir, err := os.MkdirTemp("/tmp", "ppz-read-inbox-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	requests := serveReadInboxAliasDaemon(t, sock, "foo")

	if err := runRead("inbox", true, false, false, false, false, false, 0, 0, 0); err != nil {
		t.Fatalf("runRead inbox: %v", err)
	}

	if len(*requests) != 1 {
		t.Fatalf("read request count = %d, want 1", len(*requests))
	}
	got := (*requests)[0]
	if got.Handle != "foo" || got.Channel != "inbox" {
		t.Fatalf("read inbox resolved to %q.%q, want foo.inbox", got.Handle, got.Channel)
	}
	if got.Session != "read-inbox-test" {
		t.Fatalf("read session = %q, want read-inbox-test", got.Session)
	}
}

func TestRunRead_BareInboxPrefersEnvCurrentHandle(t *testing.T) {
	t.Setenv("PPZ_SESSION", "read-inbox-test")
	t.Setenv("PPZ_CURRENT_HANDLE", "env-current")
	dir, err := os.MkdirTemp("/tmp", "ppz-read-inbox-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	requests := serveReadInboxAliasDaemon(t, sock, "daemon-current")

	if err := runRead("inbox", true, false, false, false, false, false, 0, 0, 0); err != nil {
		t.Fatalf("runRead inbox: %v", err)
	}

	if len(*requests) != 1 {
		t.Fatalf("read request count = %d, want 1", len(*requests))
	}
	got := (*requests)[0]
	if got.Handle != "env-current" || got.Channel != "inbox" {
		t.Fatalf("read inbox resolved to %q.%q, want env-current.inbox", got.Handle, got.Channel)
	}
}

func TestRunReread_BareInboxPrefersEnvCurrentHandle(t *testing.T) {
	t.Setenv("PPZ_SESSION", "read-inbox-test")
	t.Setenv("PPZ_CURRENT_HANDLE", "env-current")
	dir, err := os.MkdirTemp("/tmp", "ppz-reread-inbox-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	requests := serveReadInboxAliasDaemon(t, sock, "daemon-current")

	if err := runRead("inbox", true, false, false, false, false, true, 0, 0, 0); err != nil {
		t.Fatalf("runRead reread inbox: %v", err)
	}

	if len(*requests) != 1 {
		t.Fatalf("read request count = %d, want 1", len(*requests))
	}
	got := (*requests)[0]
	if got.Handle != "env-current" || got.Channel != "inbox" || !got.All {
		t.Fatalf("reread inbox request = handle=%q channel=%q all=%v, want env-current.inbox all=true",
			got.Handle, got.Channel, got.All)
	}
}

func TestRunRead_BareInboxWithoutCurrentSourceErrors(t *testing.T) {
	t.Setenv("PPZ_SESSION", "read-inbox-test")
	dir, err := os.MkdirTemp("/tmp", "ppz-read-inbox-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	_ = serveReadInboxAliasDaemon(t, sock, "")

	readErr := runRead("inbox", true, false, false, false, false, false, 0, 0, 0)
	var pErr *cliproto.Error
	if !errors.As(readErr, &pErr) || pErr.Code != cliproto.ENoCurrentSource {
		t.Fatalf("runRead inbox without current error = %#v, want %s", readErr, cliproto.ENoCurrentSource)
	}
}

// TestRunRead_ForwardsEnvCurrentHandleAsSenderHint repros the
// shared-pty ack-no-sender bug:
//
//	$ ppz terminal share alan                        # outer
//	alan@... % ppz read alan.inbox                   # drains, daemon emits ack
//	(jimmy reads inbox; sees "ack:read → ba39506c" with sender="")
//
// `ppz terminal share H` exports PPZ_CURRENT_HANDLE=H + PPZ_SESSION=H
// into the wrapped shell, but the daemon's IPCCreate skips
// SetCurrent for PTY-kind sources — so State.Current(req.Session)
// is empty when the wrapped child reads. The ack auto-emitter at
// read.go:316,371 stamps envelope.sender = State.Current(req.Session)
// directly, missing the env hint every other CLI verb honours.
//
// Fix shape (same as PR #92 for send): CLI forwards PPZ_CURRENT_HANDLE
// as ReadRequest.Sender; daemon's emit sites route through
// senderForRequest to apply hint-wins precedence.
//
// RED today — CLI doesn't populate ReadRequest.Sender.
func TestRunRead_ForwardsEnvCurrentHandleAsSenderHint(t *testing.T) {
	t.Setenv("PPZ_SESSION", "alan")
	t.Setenv("PPZ_CURRENT_HANDLE", "alan")
	dir, err := os.MkdirTemp("/tmp", "ppz-read-sender-hint-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	requests := serveReadInboxAliasDaemon(t, sock, "")

	// Read a concrete target (not bare `inbox`) — the bug surfaces on
	// any read, not just `read inbox`. The Sender hint is independent
	// of Handle/Channel resolution; it's purely the reader's identity
	// for ack emission.
	if err := runRead("alan.inbox", true, false, false, false, false, false, 0, 0, 0); err != nil {
		t.Fatalf("runRead: %v", err)
	}
	if len(*requests) != 1 {
		t.Fatalf("ReadRequest count = %d, want 1", len(*requests))
	}
	got := (*requests)[0]
	if got.Sender != "alan" {
		t.Fatalf("ReadRequest.Sender = %q, want %q — PPZ_CURRENT_HANDLE=alan must be forwarded as the reader's identity so the daemon's ack:read emission can stamp envelope.sender=alan instead of \"\"",
			got.Sender, "alan")
	}
}

// TestRunRead_NoEnvCurrent_OmitsSenderHint regression-pins the
// no-env path: when PPZ_CURRENT_HANDLE is unset, the CLI does NOT
// proactively populate Sender — the daemon's State.Current(session)
// fallback (via senderForRequest) handles that case at the emit
// site. Mirrors TestCmdSend_NoEnvCurrent_OmitsSenderHint.
func TestRunRead_NoEnvCurrent_OmitsSenderHint(t *testing.T) {
	t.Setenv("PPZ_CURRENT_HANDLE", "")
	_ = os.Unsetenv("PPZ_CURRENT_HANDLE")
	t.Setenv("PPZ_SESSION", "tty-no-env-read")
	dir, err := os.MkdirTemp("/tmp", "ppz-read-no-env-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	requests := serveReadInboxAliasDaemon(t, sock, "")

	if err := runRead("alan.inbox", true, false, false, false, false, false, 0, 0, 0); err != nil {
		t.Fatalf("runRead: %v", err)
	}
	if len(*requests) != 1 {
		t.Fatalf("ReadRequest count = %d, want 1", len(*requests))
	}
	got := (*requests)[0]
	if got.Sender != "" {
		t.Fatalf("ReadRequest.Sender = %q, want \"\" — no env override means no hint; daemon falls back to State.Current(session) via senderForRequest",
			got.Sender)
	}
}

func serveReadInboxAliasDaemon(t *testing.T, sock, current string) *[]cliproto.ReadRequest {
	t.Helper()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen fake daemon: %v", err)
	}
	var readRequests []cliproto.ReadRequest
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
			case cliproto.IPCRead:
				var rr cliproto.ReadRequest
				_ = json.Unmarshal(req.Params, &rr)
				readRequests = append(readRequests, rr)
			}
			_ = conn.Close()
		}
	}()

	return &readRequests
}
