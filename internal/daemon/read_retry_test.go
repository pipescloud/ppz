package daemon

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// wedgeableProxy is a TCP proxy that can be "wedged": after Wedge(), it
// keeps both sides of every proxied connection open but silently swallows
// all traffic. To a NATS client this is the production stall shape — the
// socket is CONNECTED, requests go out, replies never come — without the
// connection ever erroring. This is how the 2026-08-19 incident looked
// from the daemon: a JetStream stream-check timing out on a conn whose
// Status() was still CONNECTED.
type wedgeableProxy struct {
	ln      net.Listener
	backend string
	wedged  atomic.Bool
}

func startWedgeableProxy(t *testing.T, backendURL string) *wedgeableProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &wedgeableProxy{ln: ln, backend: strings.TrimPrefix(backendURL, "nats://")}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b, err := net.Dial("tcp", p.backend)
			if err != nil {
				c.Close()
				continue
			}
			go p.pipe(c, b)
			go p.pipe(b, c)
		}
	}()
	return p
}

func (p *wedgeableProxy) URL() string { return "nats://" + p.ln.Addr().String() }
func (p *wedgeableProxy) Wedge()      { p.wedged.Store(true) }

// pipe copies src→dst until either side errors. While wedged, bytes are
// read and dropped: nothing is forwarded, nothing is closed.
func (p *wedgeableProxy) pipe(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if p.wedged.Load() {
			continue
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

// TestHandleRead_RetriesOnceAcrossRebuild pins fix 4 from the 2026-08-19
// incident: the command that trips a JetStream transport timeout must
// not be the one command guaranteed to fail. Today handleRead's stream
// check reports the failure (closing/rebuilding the connection) and then
// immediately returns E_NATS_UNREACHABLE — even though the daemon is
// healthy again ~1s later, which is why the reporter's manual retry at
// +2s always succeeded. The daemon should absorb that window itself:
// after a transport-shaped stream-check failure, re-run ensureNATS and
// retry the read ONCE on the rebuilt connection before giving up.
//
// Retry scope mirrors the incident report's constraint: transport-gone
// only (timeout / connection closed / no servers). A definitive refusal
// (stream not found, invalid pipe) is not retried — reads are idempotent
// so the retry itself is always safe, but retrying refusals only adds
// latency to genuine errors.
//
// Topology: the daemon's LIVE conn goes through a wedgeable proxy; the
// stream check stalls into a timeout on a conn that still reports
// CONNECTED (the incident shape). d.NATSURL points directly at the
// healthy server, so the rebuild lands on a working connection and the
// retried stream check succeeds.
//
// RED: handleRead returns E_NATS_UNREACHABLE from the first attempt.
// GREEN: the caller sees the message — the blip is invisible.
func TestHandleRead_RetriesOnceAcrossRebuild(t *testing.T) {
	realURL := startEmbeddedJSURL(t)
	proxy := startWedgeableProxy(t, realURL)

	// Shrink the deadline-less JetStream API timeout so the wedged
	// stream check fails in ~250ms, not 5s (jsAPITimeout is the seam the
	// GREEN change should also bound its verification probe with).
	prevAPI := jsAPITimeout
	jsAPITimeout = 250 * time.Millisecond
	t.Cleanup(func() { jsAPITimeout = prevAPI })

	d := New(t.TempDir(), "")
	loginForWakeTests(t, d)
	d.NATSURL = realURL // rebuild path dials the healthy server directly
	d.reconnectBackoff = 5 * time.Millisecond
	d.dial = func(u string, _ *RefreshLoop, store func(NATSEvent)) (*nats.Conn, error) {
		return nats.Connect(u, natsObserveOptions(store, nil)...)
	}
	d.Refresh = &RefreshLoop{
		AccountID: "00000000-0000-0000-0000-000000000001",
		Refresh: func(context.Context, string) (string, string, int64, error) {
			return "jwt", "seed", time.Now().Add(5 * time.Minute).Unix(), nil
		},
	}
	if err := d.Refresh.Start(context.Background(), "jwt", "seed", time.Now().Add(5*time.Minute).Unix()); err != nil {
		t.Fatalf("RefreshLoop.Start: %v", err)
	}
	t.Cleanup(d.Refresh.Stop)

	// Seed the uncollared pipe "lobby" (default namespace) with one
	// retained message, via a direct connection to the healthy server.
	accountID := uuid.MustParse(d.State.AccountID())
	subject := natsubj.BuildSubject(accountID, "", "", "lobby")
	seedNC, err := nats.Connect(realURL)
	if err != nil {
		t.Fatalf("seed connect: %v", err)
	}
	t.Cleanup(seedNC.Close)
	seedJS, err := jetstream.New(seedNC)
	if err != nil {
		t.Fatalf("seed jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := seedJS.CreateStream(ctx, jetstream.StreamConfig{
		Name:     natsubj.BuildStreamName(accountID, "", "", "lobby"),
		Subjects: []string{subject},
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	body, _ := json.Marshal(envelope.New("eve", "", "hello across the rebuild", time.Now()))
	if _, err := seedJS.Publish(ctx, subject, body); err != nil {
		t.Fatalf("seed publish: %v", err)
	}

	// Install the daemon's live connection THROUGH the proxy, then wedge
	// it. The conn stays CONNECTED (nothing errors; default ping
	// interval is minutes away) but every JetStream request will stall —
	// so handleRead's ensureNATS coalesces onto it (connected + current
	// JWT generation) and the stream check times out mid-command,
	// exactly the incident interleaving.
	wedgedNC, err := nats.Connect(proxy.URL(), natsObserveOptions(d.recordNATSEvent, nil)...)
	if err != nil {
		t.Fatalf("connect via proxy: %v", err)
	}
	t.Cleanup(wedgedNC.Close)
	d.swapNC("test-initial", wedgedNC)
	proxy.Wedge()
	if !wedgedNC.IsConnected() {
		t.Fatalf("precondition: wedged conn must still report CONNECTED")
	}

	ev, panicked := callRead(t, d, cliproto.ReadRequest{
		BareTarget: "lobby",
		Session:    "retry-test",
	})
	if panicked != nil {
		t.Fatalf("handleRead panicked: %v", panicked)
	}
	if ev.Error != nil {
		t.Fatalf("handleRead returned %s after the transport blip — expected the daemon to rebuild and retry the read once (the reporter's manual +2s retry always succeeded; the daemon must absorb that window itself)", ev.Error.Code)
	}
	if ev.Message == nil || ev.Message.Payload != "hello across the rebuild" {
		t.Fatalf("expected the seeded message after the retried read, got %+v", ev)
	}
}
