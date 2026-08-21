package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// TestHandleRead_Follow_DoesNotReplayPastCursor pins the half of once-only
// .stdin delivery that the cursor alone does not buy.
//
// handleRead assigns lastSeqSeen only inside the retained-drain loop
// (read.go:316). On a resume the stored cursor already covers the whole
// retained window, so startSeq = cursor+1 > LastSeq, the drain block is
// skipped entirely, and lastSeqSeen is still 0 when the live consumer opens
// with OptStartSeq: lastSeqSeen + 1 == 1 (read.go:404) — replaying the whole
// stream as "live" messages. The cursor advanced correctly and was consulted
// correctly; it just never reached the one line that decides where the live
// consumer starts.
//
// This is the `ppz terminal share` resume case: a second share process on a
// handle whose .stdin cursor is fully caught up gets every retained command
// re-fed, and its seenIDRing is empty because it is a new process.
//
// RED: session 2 receives the 2 messages session 1 already consumed.
// GREEN: session 2 receives nothing.
func TestHandleRead_Follow_DoesNotReplayPastCursor(t *testing.T) {
	sockPath, publish := newFollowCursorFixture(t)

	// Two envelopes, exactly as `ppz command <h> "CMD1" --newline` lands:
	// the instruction, then the submit byte.
	publish("CMD1", "\n")

	follow := followCollector(t, sockPath)

	// Session 1: the live agent consumes both messages and the daemon
	// advances the cursor as it writes them to the socket.
	first := follow(cliproto.ReadRequest{BareTarget: "stdin", Session: "agent-session", Follow: true}, 700*time.Millisecond)
	if len(first) != 2 {
		t.Fatalf("session 1: got %d messages %q, want the 2 retained", len(first), first)
	}

	// Session 2: a NEW share process on the same handle. The cursor is
	// caught up, so nothing is owed to it.
	second := follow(cliproto.ReadRequest{BareTarget: "stdin", Session: "agent-session", Follow: true}, 700*time.Millisecond)
	if len(second) != 0 {
		t.Errorf("session 2 replayed %d already-consumed message(s) %q — a resumed pty session re-feeds its child every retained command", len(second), second)
	}
}

// TestHandleRead_Follow_SeedLatest_SkipsBacklogOnFirstRead pins the upgrade
// transition, which the cursor alone cannot cover: a watermark only starts
// protecting a handle once one has been written.
//
// The first `ppz terminal share` on a handle after upgrading to
// cursor-advancing delivery has no stored entry, so it drains the entire
// retained window into the child — up to 24h of commands the agent already
// ran, which is precisely the field incident. Same for a wiped PPZ_HOME, a
// different machine, or a brand-new handle that was sent commands before it
// ever existed.
//
// SeedLatest says "no stored cursor means caught up, not empty": treat
// LastSeq as already consumed and persist it, so the host starts listening
// rather than replaying. Only the pty host sets it — for `ppz read` a fresh
// session SHOULD see retained history, which is the whole unread model.
//
// RED: the first follow drains the backlog.
// GREEN: it delivers nothing, and a later live message still arrives.
func TestHandleRead_Follow_SeedLatest_SkipsBacklogOnFirstRead(t *testing.T) {
	sockPath, publish := newFollowCursorFixture(t)

	// A backlog that predates any share process on this handle.
	publish("STALE1", "\n", "STALE2", "\n")

	follow := followCollector(t, sockPath)
	got := follow(cliproto.ReadRequest{
		BareTarget: "stdin",
		Session:    "fresh-host",
		Follow:     true,
		SeedLatest: true,
	}, 700*time.Millisecond)
	if len(got) != 0 {
		t.Errorf("first follow drained %d backlog message(s) %q into the child — the upgrade transition replays the very incident the cursor is meant to stop", len(got), got)
	}

	// The seed must not deafen the host: a command issued after it starts
	// still has to arrive.
	done := make(chan []string, 1)
	go func() {
		done <- follow(cliproto.ReadRequest{
			BareTarget: "stdin",
			Session:    "fresh-host",
			Follow:     true,
			SeedLatest: true,
		}, 900*time.Millisecond)
	}()
	time.Sleep(250 * time.Millisecond)
	publish("LIVE")
	if live := <-done; len(live) != 1 || live[0] != "LIVE" {
		t.Errorf("after seeding, live delivery got %q, want [LIVE]", live)
	}
}

// newFollowCursorFixture stands up a daemon wired to an embedded
// JetStream, returns its IPC socket path and a publisher for the uncollared
// "stdin" pipe. The uncollared (BareTarget) path skips handleRead's HTTP
// source verification; the cursor / OptStartSeq logic under test is shared
// with the collared <handle>.<channel> path the pty host uses.
func newFollowCursorFixture(t *testing.T) (string, func(payloads ...string)) {
	t.Helper()
	nc := startEmbeddedJS(t)

	home, err := os.MkdirTemp("/tmp", "ppz-follow-cursor-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	sockPath := filepath.Join(home, "daemon.sock")
	d := New(home, sockPath)

	loginForWakeTests(t, d)
	d.Refresh = &RefreshLoop{
		AccountID: "00000000-0000-0000-0000-000000000001",
		Refresh: func(_ context.Context, _ string) (string, string, int64, error) {
			return "jwt", "seed", time.Now().Add(time.Hour).Unix(), nil
		},
	}
	if err := d.Refresh.Start(context.Background(), "jwt", "seed", time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("RefreshLoop.Start: %v", err)
	}
	t.Cleanup(d.Refresh.Stop)
	d.NATSURL = nc.ConnectedUrl()
	d.swapNC("test-init", nc)

	accountID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	ctx := context.Background()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	manifold := d.State.CurrentNamespace("agent-session")
	streamName := natsubj.BuildStreamName(accountID, manifold, "", "stdin")
	subject := natsubj.BuildSubject(accountID, manifold, "", "stdin")
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}

	daemonCtx, daemonCancel := context.WithCancel(context.Background())
	t.Cleanup(daemonCancel)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen unix %s: %v", sockPath, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go d.serveIPC(daemonCtx, ln)

	publish := func(payloads ...string) {
		t.Helper()
		for _, payload := range payloads {
			env := envelope.New("operator", "", payload, time.Now())
			b, merr := env.Marshal()
			if merr != nil {
				t.Fatalf("marshal envelope: %v", merr)
			}
			if _, perr := js.Publish(ctx, subject, b); perr != nil {
				t.Fatalf("publish: %v", perr)
			}
		}
	}
	return sockPath, publish
}

// followCollector returns a function that opens one follow with the given
// request and collects whatever arrives within `window`, mirroring
// streamForwardStdinOnce's connection lifecycle.
func followCollector(t *testing.T, sockPath string) func(cliproto.ReadRequest, time.Duration) []string {
	t.Helper()
	return func(req cliproto.ReadRequest, window time.Duration) []string {
		conn, derr := net.Dial("unix", sockPath)
		if derr != nil {
			t.Fatalf("dial IPC: %v", derr)
		}
		defer conn.Close()

		body, _ := json.Marshal(req)
		if eerr := json.NewEncoder(conn).Encode(map[string]any{
			"method": cliproto.IPCRead,
			"params": json.RawMessage(body),
		}); eerr != nil {
			t.Fatalf("encode Follow request: %v", eerr)
		}

		var got []string
		done := make(chan struct{})
		go func() {
			defer close(done)
			sc := bufio.NewScanner(conn)
			sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
			for sc.Scan() {
				var evt cliproto.ReadEvent
				if json.Unmarshal(sc.Bytes(), &evt) != nil || evt.Message == nil {
					continue
				}
				got = append(got, evt.Message.Payload)
			}
		}()
		time.Sleep(window)
		_ = conn.Close()
		<-done
		return got
	}
}
