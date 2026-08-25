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

// TestHandleRead_Follow_SeedSince_DropsBacklogButNotTheConnectRace pins the
// upgrade transition WITHOUT swallowing live input.
//
// A watermark only protects a handle once one has been written, so the first
// share on a handle — after an upgrade, a wiped PPZ_HOME, a new machine, a
// brand-new handle — has no cursor and would drain the pipe's whole 24h
// retention into the child. But "no cursor means caught up" (stamp the
// cursor at LastSeq) over-corrects: it also discards anything published in
// the window between the pipe being provisioned and the host actually
// connecting, which is open on every ordinary share startup. That regressed
// lease/no-lease-stdin-passes-through (a send racing `terminal share`) and
// terminal/share-stdin-survives-share-daemon-logout (logout does
// RemoveAll(<home>/cursors), so the host redials with no watermark).
//
// The floor is therefore a TIME — when the host started following — not a
// sequence. Older than that is a backlog the agent has already run; newer is
// input someone issued to this host, whether or not it beat the connect.
//
// RED: the connect-race message is dropped along with the backlog.
// GREEN: only the backlog is dropped.
func TestHandleRead_Follow_SeedSince_DropsBacklogButNotTheConnectRace(t *testing.T) {
	sockPath, publish := newFollowCursorFixture(t)

	// A backlog that predates the host: commands issued to an earlier
	// incarnation of this agent, which already ran them.
	publish("STALE1", "\n", "STALE2", "\n")

	// The host starts following here.
	time.Sleep(50 * time.Millisecond)
	hostStart := time.Now()
	time.Sleep(50 * time.Millisecond)

	// A command issued after the host started but before its follow is
	// established — `ppz terminal share agent & ppz command agent ...`, or
	// any send that beats the dial. This one is owed to the child.
	publish("RACE")

	follow := followCollector(t, sockPath)
	got := follow(cliproto.ReadRequest{
		BareTarget:      "stdin",
		Session:         "fresh-host",
		Follow:          true,
		SeedSinceUnixMS: hostStart.UnixMilli(),
	}, 700*time.Millisecond)

	if len(got) != 1 || got[0] != "RACE" {
		t.Errorf("first follow delivered %q, want [RACE] — the backlog must be dropped and the connect-race message must not be", got)
	}

	// And it must still be listening afterwards.
	done := make(chan []string, 1)
	go func() {
		done <- follow(cliproto.ReadRequest{
			BareTarget:      "stdin",
			Session:         "fresh-host",
			Follow:          true,
			SeedSinceUnixMS: hostStart.UnixMilli(),
		}, 900*time.Millisecond)
	}()
	time.Sleep(250 * time.Millisecond)
	publish("LIVE")
	if live := <-done; len(live) == 0 || live[len(live)-1] != "LIVE" {
		t.Errorf("after seeding, live delivery got %q, want it to end with LIVE", live)
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
