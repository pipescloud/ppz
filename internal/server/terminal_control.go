package server

// Browser control of a shared terminal, multiplexed onto the terminal
// WebSocket (handlers_terminal.go).
//
// Access is the ACL's: a viewer may take control iff they may write the
// source's stdin (sessionCan(…, "<handle>.stdin", acl.Write)), which is
// every member while the org hasn't opted into enforcement — the same as
// `ppz terminal control`. Re-checked on every lease renewal, so revoking
// write ends a session within one renewal period.
//
// Mechanics mirror `ppz terminal control` so the pty host can't tell the
// difference: the server acquires the advisory write-lease on
// <handle>.system for the browser (30s, renewed every 10s, released on
// close), and only while it holds the lease forwards keystrokes to
// <handle>.stdin as envelopes stamped with the controller's sender. The
// host drops non-holder input while a lease is held (see docs/WIRE.md
// §12), so two controllers can't interleave.
//
// Wire (browser ⇄ server), alongside the existing stdout/stdctrl frames:
//
//	browser → server  text   {"type":"take"} | {"type":"release"}
//	browser → server  text   {"type":"setsize","cols":C,"rows":R}
//	browser → server  binary keystroke bytes (UTF-8 from xterm onData)
//	server → browser  text   {"type":"control","writable":B,"resizable":B,"you":S}
//	server → browser  text   {"type":"lease","holder":H,"until":T}
//	server → browser  text   {"type":"control-error","error":E}

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/acl"
	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

const (
	// Same windows as `ppz terminal control` (internal/cli/terminal.go).
	guiLeaseTTL   = 30 * time.Second
	guiLeaseRenew = 10 * time.Second
	guiLeaseWait  = 5 * time.Second
	// guiInputChunk caps one stdin envelope, like the CLI's 4 KiB reads.
	guiInputChunk = 4096
)

// guiLeaseMsg is the <handle>.system payload shape (cli.leaseMsg).
type guiLeaseMsg struct {
	Type       string `json:"type"`
	TTLMs      int64  `json:"ttl_ms,omitempty"`
	Holder     string `json:"holder,omitempty"`
	Until      string `json:"until,omitempty"`
	Nonce      string `json:"nonce,omitempty"`
	ReplyNonce string `json:"reply_nonce,omitempty"`
}

type terminalController struct {
	s      *Server
	js     jetstream.JetStream
	org    db.Account
	src    db.Source
	userID uuid.UUID
	// sender is the identity the lease and stdin envelopes carry. A
	// distinct "@web" form keeps a browser session from silently sharing
	// a lease with the same user's CLI under their handle.
	sender string
	conn   *websocket.Conn

	mu        sync.Mutex
	holding   bool
	pending   map[string]chan string // nonce → holder the host reported
	stopRenew context.CancelFunc
}

func newTerminalController(s *Server, js jetstream.JetStream, org db.Account, src db.Source, userID uuid.UUID, username string, conn *websocket.Conn) *terminalController {
	return &terminalController{
		s: s, js: js, org: org, src: src, userID: userID,
		sender:  username + "@web",
		conn:    conn,
		pending: map[string]chan string{},
	}
}

func (c *terminalController) canWrite(ctx context.Context) bool {
	ok, err := c.s.sessionCan(ctx, c.org.ID, c.userID, c.src.Handle+".stdin", acl.Write)
	return err == nil && ok
}

// canResize: resizing publishes to stdctrl, so it needs write there. It
// doesn't need the control lease — it changes the view, not the input.
func (c *terminalController) canResize(ctx context.Context) bool {
	ok, err := c.s.sessionCan(ctx, c.org.ID, c.userID, c.src.Handle+".stdctrl", acl.Write)
	return err == nil && ok
}

// maxTermDim bounds a requested dimension (pty sizes are uint16; anything
// near that is a bug, not a terminal). Matches the CLI's bound.
const maxTermDim = 1000

// setsize asks the pty host to resize the child, exactly as
// `ppz terminal resize` does: a "setsize" on <handle>.stdctrl, which the
// host applies and answers with a "resize" every viewer follows.
func (c *terminalController) setsize(ctx context.Context, cols, rows int) {
	if cols < 1 || rows < 1 || cols > maxTermDim || rows > maxTermDim {
		c.sendError(ctx, "invalid size")
		return
	}
	if !c.canResize(ctx) {
		c.sendError(ctx, "you don't have write access to "+c.src.Handle+".stdctrl")
		return
	}
	payload, _ := json.Marshal(map[string]any{"type": "setsize", "cols": cols, "rows": rows})
	if err := c.publish(ctx, "stdctrl", string(payload)); err != nil {
		c.sendError(ctx, "resize not delivered: "+err.Error())
	}
}

func (c *terminalController) send(ctx context.Context, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = c.conn.Write(wctx, websocket.MessageText, b)
}

func (c *terminalController) sendError(ctx context.Context, msg string) {
	c.send(ctx, map[string]string{"type": "control-error", "error": msg})
}

func (c *terminalController) publish(ctx context.Context, pipe, payload string) error {
	env := envelope.New(c.sender, "", payload, time.Now().UTC())
	data, err := env.Marshal()
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = c.js.Publish(pctx, natsubj.BuildSubject(c.org.ID, c.src.Manifold, c.src.Handle, pipe), data)
	return err
}

func (c *terminalController) publishLease(ctx context.Context, typ string) (string, error) {
	nonce := guiLeaseNonce()
	m := guiLeaseMsg{Type: typ, Nonce: nonce}
	if typ == "lease-acquire" {
		m.TTLMs = guiLeaseTTL.Milliseconds()
	}
	payload, _ := json.Marshal(m)
	return nonce, c.publish(ctx, "system", string(payload))
}

// start announces the viewer's capability and current lease, then follows
// <handle>.system so lease changes reach the browser and acquire replies
// reach take(). Returns a stop func. A source without a system stream
// (message sources, very old shares) can't be controlled.
func (c *terminalController) start(ctx context.Context) func() {
	stream, err := c.js.Stream(ctx, natsubj.BuildStreamName(c.org.ID, c.src.Manifold, c.src.Handle, "system"))
	if err != nil {
		c.send(ctx, map[string]any{"type": "control", "writable": false, "resizable": c.canResize(ctx), "you": c.sender})
		return func() {}
	}
	c.send(ctx, map[string]any{"type": "control", "writable": c.canWrite(ctx), "resizable": c.canResize(ctx), "you": c.sender})

	var startSeq uint64 = 1
	if info, err := stream.Info(ctx); err == nil {
		startSeq = info.State.LastSeq + 1
		// Current holder, if the latest system message says one is live.
		if info.State.Msgs > 0 {
			if msg, err := stream.GetMsg(ctx, info.State.LastSeq); err == nil {
				if env, err := envelope.Unmarshal(msg.Data); err == nil {
					var m guiLeaseMsg
					if json.Unmarshal([]byte(env.Payload), &m) == nil && m.Type == "lease-state" {
						if until, err := time.Parse(time.RFC3339, m.Until); err == nil && until.After(time.Now()) {
							c.send(ctx, map[string]string{"type": "lease", "holder": m.Holder, "until": m.Until})
						}
					}
				}
			}
		}
	}
	consumer, err := stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{natsubj.BuildSubject(c.org.ID, c.src.Manifold, c.src.Handle, "system")},
		DeliverPolicy:  jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:    startSeq,
	})
	if err != nil {
		return func() {}
	}
	cc, err := consumer.Consume(func(msg jetstream.Msg) {
		_ = msg.Ack()
		env, err := envelope.Unmarshal(msg.Data())
		if err != nil {
			return
		}
		var m guiLeaseMsg
		if json.Unmarshal([]byte(env.Payload), &m) != nil || m.Type != "lease-state" {
			return
		}
		c.mu.Lock()
		if ch, ok := c.pending[m.ReplyNonce]; ok && m.ReplyNonce != "" {
			ch <- m.Holder
			delete(c.pending, m.ReplyNonce)
		}
		lost := c.holding && m.Holder != c.sender
		if lost {
			c.holding = false
			c.stopRenewLocked()
		}
		c.mu.Unlock()
		c.send(ctx, map[string]string{"type": "lease", "holder": m.Holder, "until": m.Until})
	})
	if err != nil {
		return func() {}
	}
	return cc.Stop
}

// acquire publishes a lease-acquire and waits for the host's verdict.
func (c *terminalController) acquire(ctx context.Context) (holder string, ok bool) {
	ch := make(chan string, 1)
	nonce := guiLeaseNonce()
	c.mu.Lock()
	c.pending[nonce] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, nonce)
		c.mu.Unlock()
	}()
	m := guiLeaseMsg{Type: "lease-acquire", TTLMs: guiLeaseTTL.Milliseconds(), Nonce: nonce}
	payload, _ := json.Marshal(m)
	if err := c.publish(ctx, "system", string(payload)); err != nil {
		return "", false
	}
	select {
	case h := <-ch:
		return h, true
	case <-time.After(guiLeaseWait):
		return "", false
	case <-ctx.Done():
		return "", false
	}
}

func (c *terminalController) take(ctx context.Context) {
	if !c.canWrite(ctx) {
		c.sendError(ctx, "you don't have write access to "+c.src.Handle+".stdin")
		return
	}
	holder, ok := c.acquire(ctx)
	switch {
	case !ok:
		c.sendError(ctx, "the terminal's host didn't answer — is the share still running?")
		return
	case holder != c.sender:
		c.sendError(ctx, "controlled by "+holder)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.holding {
		return
	}
	c.holding = true
	rctx, cancel := context.WithCancel(ctx)
	c.stopRenew = cancel
	go c.renew(rctx, ctx)
}

// renew keeps the lease alive while held, dropping it if write access
// has been revoked since control was taken. ctx is cancelled by release;
// wsCtx outlives it so the release itself can still be published.
func (c *terminalController) renew(ctx, wsCtx context.Context) {
	t := time.NewTicker(guiLeaseRenew)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !c.canWrite(ctx) {
			c.release(wsCtx)
			c.sendError(wsCtx, "write access to "+c.src.Handle+".stdin was revoked")
			return
		}
		_, _ = c.publishLease(ctx, "lease-acquire")
	}
}

func (c *terminalController) stopRenewLocked() {
	if c.stopRenew != nil {
		c.stopRenew()
		c.stopRenew = nil
	}
}

func (c *terminalController) release(ctx context.Context) {
	c.mu.Lock()
	was := c.holding
	c.holding = false
	c.stopRenewLocked()
	c.mu.Unlock()
	if was {
		_, _ = c.publishLease(ctx, "lease-release")
	}
}

// input forwards keystrokes while this browser holds the lease. Bytes
// arriving without the lease are dropped: the host would drop them too
// while someone else holds it, and with no lease held they'd bypass the
// arbitration the take button exists for.
func (c *terminalController) input(ctx context.Context, b []byte) {
	c.mu.Lock()
	holding := c.holding
	c.mu.Unlock()
	if !holding {
		return
	}
	for len(b) > 0 {
		n := len(b)
		if n > guiInputChunk {
			n = guiInputChunk
		}
		if err := c.publish(ctx, "stdin", string(b[:n])); err != nil {
			c.sendError(ctx, "input not delivered: "+err.Error())
			return
		}
		b = b[n:]
	}
}

// handleFrame dispatches one browser frame.
func (c *terminalController) handleFrame(ctx context.Context, typ websocket.MessageType, data []byte) {
	if typ == websocket.MessageBinary {
		c.input(ctx, data)
		return
	}
	var m struct {
		Type string `json:"type"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}
	if json.Unmarshal(data, &m) != nil {
		return
	}
	switch m.Type {
	case "take":
		c.take(ctx)
	case "release":
		c.release(ctx)
	case "setsize":
		c.setsize(ctx, m.Cols, m.Rows)
	}
}

// close releases a held lease on disconnect. Uses a fresh context: the
// request's is already cancelled by the time the socket is gone.
func (c *terminalController) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.release(ctx)
}

func guiLeaseNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "n"
	}
	return hex.EncodeToString(b[:])
}
