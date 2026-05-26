package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// TestCall_DoesNotHangWhenDaemonAcceptsButNeverReplies is the unit-level
// reproduction of the production "ppz send hangs forever" report
// (copilot/alex session, 2026-05-26): during a ppz-server restart the
// daemon accepts the IPC connection but its handleSend blocks inside
// nats.Conn.Flush() waiting for a PONG that never comes, so it never
// writes an IPC reply. The CLI's daemon.Call has NO read deadline, so
// dec.Decode(&resp) blocks indefinitely — the user observed >2 minutes
// of silence with no output and no error.
//
// `ppz read` did not hang because it is served from local daemon state
// with no NATS round-trip; only verbs that reach the server (send) stall.
//
// This test stands up a fake daemon that accepts the connection and then
// goes silent — exactly the stalled-handler condition — and asserts that
// Call RETURNS (with an error) rather than blocking. It is the contract
// "the IPC client must bound its own wait so a stuck daemon can never
// hang the CLI".
//
// RED: with no deadline in Call, the goroutine never sends on done and we
// fall through to the timeout branch and fail.
// GREEN: a read deadline (or context) in Call makes it return an error
// within the bound.
func TestCall_DoesNotHangWhenDaemonAcceptsButNeverReplies(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ppz-ipc-hang-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen fake daemon: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// Accept the connection but never reply — model a daemon whose
	// handler is wedged inside NC.Flush() during a server outage. Hold
	// the conn open (don't close) so the client sees a live-but-silent
	// peer, not EOF.
	held := make(chan net.Conn, 1)
	go func() {
		conn, aErr := ln.Accept()
		if aErr != nil {
			return
		}
		held <- conn // keep it alive; closing would let Decode return early
	}()
	t.Cleanup(func() {
		select {
		case c := <-held:
			_ = c.Close()
		default:
		}
	})

	// The client must surface *something* within a generous ceiling. A
	// healthy local IPC round-trip is sub-millisecond; a send that has to
	// reach NATS is at most a couple of seconds. 10s is far past any
	// legitimate reply yet finite, so a never-returning Call is caught as
	// the hang it is rather than wedging the whole test binary.
	const ceiling = 10 * time.Second

	done := make(chan error, 1)
	go func() {
		var reply cliproto.SendReply
		done <- Call(sock, cliproto.IPCSend,
			cliproto.SendRequest{Handle: "james", Channel: "inbox", Payload: "hi"},
			&reply)
	}()

	select {
	case err := <-done:
		// Returning is the whole point; an unreachable/timeout error is the
		// correct shape. A nil error would be wrong (we never sent a reply),
		// but the hang is the bug under test, so assert only that it errored.
		if err == nil {
			t.Fatalf("Call returned nil error, but the fake daemon never sent a reply; expected a timeout/unreachable error")
		}
	case <-time.After(ceiling):
		t.Fatalf("ppz send hung: daemon.Call did not return within %s when the daemon accepted but never replied (no read deadline on the IPC connection)", ceiling)
	}
}
