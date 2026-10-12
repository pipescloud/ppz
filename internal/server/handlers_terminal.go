package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// handleGUITerminalPage renders the live read-only terminal viewer for a
// shared source. The page mounts xterm.js on #terminal and bootstraps a
// WebSocket against /terminal/ws on the same handle.
//
// The page itself is static — no per-request data beyond org + handle.
// All the live behaviour lives in /terminal/ws.
//
// Route: GET /orgs/{id}/sources/{handle}/terminal
func (s *Server) handleGUITerminalPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, ok := s.resolveMemberOrg(ctx, w, r)
	if !ok {
		return
	}
	handle := r.PathValue("handle")
	src, err := db.GetSourceByHandle(ctx, s.Pool, org.ID, handle)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.Error(w, "source not found", 404)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "terminal.html", map[string]any{
		"Org":    org,
		"Handle": src.Handle,
	}); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// handleGUITerminalWS streams .stdout bytes over a WebSocket. Drains
// retained messages first (so the page renders a faithful replay of the
// session so far), then follows live writes until the client closes.
//
// Read-only: incoming frames from the browser are discarded. Stdin will
// land in a future iteration with a write-lease design.
//
// Route: GET /orgs/{id}/sources/{handle}/terminal/ws
func (s *Server) handleGUITerminalWS(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	org, ok := s.resolveMemberOrg(ctx, w, r)
	if !ok {
		return
	}
	handle := r.PathValue("handle")
	src, err := db.GetSourceByHandle(ctx, s.Pool, org.ID, handle)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.Error(w, "source not found", 404)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}

	// stdout carries the byte stream; stdctrl carries control signals
	// (initially just JSON resize events). Both feed the same WebSocket
	// — stdout as binary frames, stdctrl as text frames.
	js, err := s.JSFor(ctx, org.ID)
	if err != nil {
		http.Error(w, "org account: "+err.Error(), 500)
		return
	}
	stdoutStream, err := js.Stream(ctx, natsubj.BuildStreamName(org.ID, src.Manifold, src.Handle, "stdout"))
	if err != nil {
		http.Error(w, "no stdout stream for this source (has it been shared yet?)", 404)
		return
	}
	// stdctrl is auto-provisioned on pty sources but may be missing on
	// older shares. A nil stdctrlStream just skips the resize plumbing —
	// xterm.js falls back to its default size, which is wrong but not
	// broken.
	stdctrlStream, _ := js.Stream(ctx, natsubj.BuildStreamName(org.ID, src.Manifold, src.Handle, "stdctrl"))

	// nil options: the library's default same-origin check. The session
	// cookie rides on cross-site WS handshakes, so without it any page the
	// user visits could attach to their org's terminals.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		// Accept already wrote a response.
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// 1) Send the latest stdctrl (if any) up front so xterm.js can size
	// itself before bytes arrive. Without this the replay renders at
	// xterm.js's default cols and lines wrap incorrectly.
	var lastStdctrlSeq uint64
	if stdctrlStream != nil {
		if info, ierr := stdctrlStream.Info(ctx); ierr == nil && info.State.Msgs > 0 {
			if msg, mErr := stdctrlStream.GetMsg(ctx, info.State.LastSeq); mErr == nil {
				if env, eErr := envelope.Unmarshal(msg.Data); eErr == nil {
					_ = conn.Write(ctx, websocket.MessageText, []byte(env.Payload))
				}
			}
			lastStdctrlSeq = info.State.LastSeq
		}
	}

	// 2) Drain retained stdout by sequence so the browser replays the
	// session from the beginning. Same shape as daemon/read.go's first
	// pass, minus the filters (no skip / since / limit on the GUI yet).
	info, err := stdoutStream.Info(ctx)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "stream info")
		return
	}
	var lastStdoutSeq uint64
	if info.State.Msgs > 0 {
		for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
			msg, err := stdoutStream.GetMsg(ctx, seq)
			if err != nil {
				continue // expired / dropped
			}
			env, err := envelope.Unmarshal(msg.Data)
			if err != nil {
				continue
			}
			if err := conn.Write(ctx, websocket.MessageBinary, []byte(env.Payload)); err != nil {
				return
			}
			lastStdoutSeq = seq
		}
	}

	// 3) Follow live for both streams. Same pattern as daemon/read.go's
	// Follow branch — one OrderedConsumer each, both forward as frames
	// and either ending tears down the conn.
	stdoutConsumer, err := stdoutStream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{natsubj.BuildSubject(org.ID, src.Manifold, src.Handle, "stdout")},
		DeliverPolicy:  jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:    lastStdoutSeq + 1,
	})
	if err != nil {
		conn.Close(websocket.StatusInternalError, "stdout consumer")
		return
	}
	stdoutCC, err := stdoutConsumer.Consume(func(msg jetstream.Msg) {
		env, err := envelope.Unmarshal(msg.Data())
		if err != nil {
			_ = msg.Ack()
			return
		}
		wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
		defer wcancel()
		if err := conn.Write(wctx, websocket.MessageBinary, []byte(env.Payload)); err != nil {
			cancel()
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return
	}
	defer stdoutCC.Stop()

	if stdctrlStream != nil {
		stdctrlConsumer, cerr := stdctrlStream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
			FilterSubjects: []string{natsubj.BuildSubject(org.ID, src.Manifold, src.Handle, "stdctrl")},
			DeliverPolicy:  jetstream.DeliverByStartSequencePolicy,
			OptStartSeq:    lastStdctrlSeq + 1,
		})
		if cerr == nil {
			stdctrlCC, ccerr := stdctrlConsumer.Consume(func(msg jetstream.Msg) {
				env, err := envelope.Unmarshal(msg.Data())
				if err != nil {
					_ = msg.Ack()
					return
				}
				wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
				defer wcancel()
				if err := conn.Write(wctx, websocket.MessageText, []byte(env.Payload)); err != nil {
					cancel()
					return
				}
				_ = msg.Ack()
			})
			if ccerr == nil {
				defer stdctrlCC.Stop()
			}
		}
	}

	// 4) Block until the client disconnects or the request ctx is cancelled.
	// Reading from the conn lets us notice both — even though we don't
	// process the messages (read-only).
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()
	<-ctx.Done()
}
