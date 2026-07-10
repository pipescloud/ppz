package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// chatMessageView is the JSON shape the browser (and the e2e suite) reads for
// every message — over both the /messages snapshot and the /ws live tail.
// `you` marks a message the current viewer sent (envelope sender == me), so
// the UI can right-align / label it without a second round-trip.
type chatMessageView struct {
	ID        string `json:"id"`
	Sender    string `json:"sender"`
	Payload   string `json:"payload"`
	CreatedAt string `json:"created_at"`
	You       bool   `json:"you"`
}

// chatView maps a wire envelope to the JSON view, stamping `you` for the
// current viewer. Shared by the snapshot, the send ack, and the live tail so
// the three can't disagree on the shape.
func chatView(env envelope.Message, me string) chatMessageView {
	return chatMessageView{
		ID:        env.ID,
		Sender:    env.Sender,
		Payload:   env.Payload,
		CreatedAt: env.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		You:       me != "" && env.Sender == me,
	}
}

// meFromCtx resolves the current session user's username — the handle stamped
// as the envelope sender on web-originated sends, and the "you" discriminator
// on reads. Returns "" if the lookup fails; send treats that as fatal (it must
// not publish an unattributed message), reads degrade to no "you" label.
func (s *Server) meFromCtx(ctx context.Context) string {
	uid := UserIDFromCtx(ctx)
	if uid == uuid.Nil {
		return ""
	}
	u, err := db.GetUser(ctx, s.Pool, uid)
	if err != nil {
		return ""
	}
	return u.Username
}

// resolveChatOrg resolves the org from the URL and confirms the session user
// may see it. Cross-tenant guard: unlike the read-only pipe/terminal pages,
// /chat is a read+write surface (send publishes into the org's streams), so it
// gates on membership. A non-member gets 404 (not 403) so org existence
// doesn't leak to outsiders.
func (s *Server) resolveChatOrg(ctx context.Context, w http.ResponseWriter, r *http.Request) (db.Account, bool) {
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("id"))
	if err != nil {
		http.Error(w, "org not found", 404)
		return db.Account{}, false
	}
	if !db.IsMemberOrOwner(ctx, s.Pool, org.ID, UserIDFromCtx(r.Context())) {
		http.Error(w, "org not found", 404)
		return db.Account{}, false
	}
	return org, true
}

// heartbeatLiveness struct-lite: only the two fields the roster needs off the
// heartbeat JSON payload. A local parse avoids importing internal/cli (which
// owns the full HeartbeatPayload) into the HTTP server.
type heartbeatLiveness struct {
	AgentState  string `json:"agent_state"`
	IntervalSec int    `json:"interval_sec"`
}

// readAgentLiveness reads a pty source's heartbeat stream's latest beat: the
// delivery time (drives online/stale/offline) plus interval + agent_state.
// ok=false when there's no beat yet (source shared but never beaten, or the
// account isn't provisioned) — the roster then renders it offline.
func readAgentLiveness(ctx context.Context, js jetstream.JetStream, streamName string) (at time.Time, intervalSec int, state string, ok bool) {
	if js == nil {
		return time.Time{}, 0, "", false
	}
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		return time.Time{}, 0, "", false
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs == 0 {
		return time.Time{}, 0, "", false
	}
	msg, err := stream.GetMsg(ctx, info.State.LastSeq)
	if err != nil {
		return time.Time{}, 0, "", false
	}
	env, err := envelope.Unmarshal(msg.Data)
	if err != nil {
		return time.Time{}, 0, "", false
	}
	var hb heartbeatLiveness
	_ = json.Unmarshal([]byte(env.Payload), &hb) // best-effort; zero values are fine
	at = info.State.LastTime
	if at.IsZero() {
		at = env.CreatedAt
	}
	return at, hb.IntervalSec, hb.AgentState, true
}

// gatherChatRoster builds the three-section roster for an org: every source
// (agents = pty, inboxes = message) plus every uncollared pipe, with agent
// liveness read from each pty source's heartbeat stream. The per-agent
// heartbeat reads (up to 3 JetStream round-trips each) run concurrently so the
// page-render latency doesn't grow linearly with the agent count.
func (s *Server) gatherChatRoster(ctx context.Context, org db.Account, now time.Time) (chatRoster, error) {
	sources, err := db.ListSourcesForOrg(ctx, s.Pool, org.ID)
	if err != nil {
		return chatRoster{}, err
	}
	pipes, err := db.ListUncollaredPipesForAccount(ctx, s.Pool, org.ID)
	if err != nil {
		return chatRoster{}, err
	}
	js, _ := s.JSFor(ctx, org.ID) // nil-tolerant: unprovisioned org still renders

	srcInputs := make([]chatSourceInput, len(sources))
	var wg sync.WaitGroup
	for i, src := range sources {
		srcInputs[i] = chatSourceInput{Source: src}
		if src.Kind != db.SourceKindPTY {
			continue
		}
		wg.Add(1)
		go func(i int, src db.Source) {
			defer wg.Done()
			hbStream := natsubj.BuildStreamName(org.ID, src.Manifold, src.Handle, "heartbeat")
			if at, iv, state, ok := readAgentLiveness(ctx, js, hbStream); ok {
				srcInputs[i].HeartbeatAt, srcInputs[i].IntervalSec, srcInputs[i].AgentState = at, iv, state
			}
		}(i, src)
	}
	wg.Wait()

	pipeInputs := make([]chatPipeInput, 0, len(pipes))
	for _, p := range pipes {
		pipeInputs = append(pipeInputs, chatPipeInput{Manifold: p.Manifold, Name: p.Name})
	}
	return buildChatRoster(srcInputs, pipeInputs, now), nil
}

// handleGUIChatPage renders the full chat console: the server-rendered roster
// plus the mount points the browser JS wires the live viewport + composer to.
//
// Route: GET /orgs/{id}/chat
func (s *Server) handleGUIChatPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, ok := s.resolveChatOrg(ctx, w, r)
	if !ok {
		return
	}
	roster, err := s.gatherChatRoster(ctx, org, time.Now())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := s.base()
	data["Org"] = org
	data["Roster"] = roster
	data["Me"] = s.meFromCtx(r.Context())
	if err := tmpl.ExecuteTemplate(w, "chat.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// resolveChatWindowDB validates a (kind,target) pair, confirms the target
// exists in this org, and returns the JetStream subject + stream name. Source
// windows are resolved against the DB row so the source's real manifold is
// used (the pure resolveChatWindow only validates their handle). On any
// failure it writes the HTTP error and returns ok=false.
func (s *Server) resolveChatWindowDB(ctx context.Context, w http.ResponseWriter, org db.Account, kind, target string) (subject, streamName string, ok bool) {
	win, err := resolveChatWindow(org.ID, kind, target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", "", false
	}
	switch win.Kind {
	case "source":
		src, err := db.GetSourceByHandle(ctx, s.Pool, org.ID, win.Handle)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				http.Error(w, "source not found", 404)
			} else {
				http.Error(w, err.Error(), 500)
			}
			return "", "", false
		}
		return natsubj.BuildSubject(org.ID, src.Manifold, src.Handle, "inbox"),
			natsubj.BuildStreamName(org.ID, src.Manifold, src.Handle, "inbox"), true
	default: // pipe
		exists, err := db.UncollaredPipeExists(ctx, s.Pool, org.ID, win.Manifold, win.Name)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return "", "", false
		}
		if !exists {
			http.Error(w, "pipe not found", 404)
			return "", "", false
		}
		return win.Subject, win.StreamName, true
	}
}

// replayStream iterates a stream's retained messages front-to-back, invoking
// fn for each decoded envelope, and returns the last sequence it reached so a
// follow can resume from there. Shared by the JSON snapshot and the WS history
// replay so the two can't drift.
func replayStream(ctx context.Context, stream jetstream.Stream, fn func(envelope.Message) error) (uint64, error) {
	info, err := stream.Info(ctx)
	if err != nil {
		return 0, err
	}
	// Empty (or fully-expired) stream: nothing to replay; follow from the
	// last-ever sequence so new publishes are picked up.
	if info.State.Msgs == 0 {
		return info.State.LastSeq, nil
	}
	var last uint64
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		msg, gerr := stream.GetMsg(ctx, seq)
		if gerr != nil {
			continue // expired / dropped
		}
		env, uerr := envelope.Unmarshal(msg.Data)
		if uerr != nil {
			continue
		}
		if err := fn(env); err != nil {
			return last, err
		}
		last = seq
	}
	return last, nil
}

// drainChatMessages reads a window's stream front-to-back into message views.
// Missing stream / unprovisioned org => empty (not an error).
func drainChatMessages(ctx context.Context, js jetstream.JetStream, streamName, me string) []chatMessageView {
	out := []chatMessageView{}
	if js == nil {
		return out
	}
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		return out
	}
	_, _ = replayStream(ctx, stream, func(env envelope.Message) error {
		out = append(out, chatView(env, me))
		return nil
	})
	return out
}

// handleGUIChatMessages returns the buffered history for one window as JSON.
// The e2e suite asserts against it directly (curl can't easily drive the
// WebSocket); the browser gets its history over the WS instead.
//
// Route: GET /orgs/{id}/chat/messages?kind=&target=
func (s *Server) handleGUIChatMessages(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, ok := s.resolveChatOrg(ctx, w, r)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	target := r.URL.Query().Get("target")
	_, streamName, ok := s.resolveChatWindowDB(ctx, w, org, kind, target)
	if !ok {
		return
	}
	me := s.meFromCtx(r.Context())
	js, _ := s.JSFor(ctx, org.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": drainChatMessages(ctx, js, streamName, me),
	})
}

// chatSendRequest is the POST body for a web-originated message.
type chatSendRequest struct {
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Payload string `json:"payload"`
}

// handleGUIChatSend publishes a message from the browser to the resolved
// window's subject, stamping the sender with the viewer's username. Mirrors
// the scheduler's durable publish (blocks for the JetStream PubAck).
//
// Route: POST /orgs/{id}/chat/send
func (s *Server) handleGUIChatSend(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, ok := s.resolveChatOrg(ctx, w, r)
	if !ok {
		return
	}
	var req chatSendRequest
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Payload == "" {
		http.Error(w, "empty payload", http.StatusBadRequest)
		return
	}
	// Don't publish an unattributed message: if we can't resolve who the
	// viewer is, fail loudly rather than write an envelope with an empty
	// sender that everyone (including the sender) would see as "(unknown)".
	me := s.meFromCtx(r.Context())
	if me == "" {
		http.Error(w, "could not resolve your identity", 500)
		return
	}
	subject, _, ok := s.resolveChatWindowDB(ctx, w, org, req.Kind, req.Target)
	if !ok {
		return
	}
	js, err := s.JSFor(ctx, org.ID)
	if err != nil {
		http.Error(w, "org account: "+err.Error(), 500)
		return
	}
	env := envelope.New(me, "", req.Payload, time.Now().UTC())
	data, err := env.Marshal()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	pubCtx, pcancel := context.WithTimeout(ctx, 5*time.Second)
	defer pcancel()
	if _, err := js.Publish(pubCtx, subject, data); err != nil {
		http.Error(w, "publish: "+err.Error(), 502)
		return
	}
	writeJSON(w, http.StatusOK, chatView(env, me))
}

// handleGUIChatWS streams a window's messages live: drains retained history
// then follows new publishes, forwarding each as a JSON text frame
// (chatMessageView). The browser dedups by id, so replaying history here is
// safe. A valid window whose stream isn't materialised yet still connects
// (empty history, then follows once messages arrive) rather than erroring.
//
// Route: GET /orgs/{id}/chat/ws?kind=&target=
func (s *Server) handleGUIChatWS(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	org, ok := s.resolveChatOrg(ctx, w, r)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	target := r.URL.Query().Get("target")
	subject, streamName, ok := s.resolveChatWindowDB(ctx, w, org, kind, target)
	if !ok {
		return
	}
	me := s.meFromCtx(r.Context())
	js, err := s.JSFor(ctx, org.ID)
	if err != nil {
		http.Error(w, "org account: "+err.Error(), 500)
		return
	}
	// The window is valid (resolveChatWindowDB confirmed the source/pipe row);
	// the backing stream may still be missing on an unprovisioned account. A
	// nil stream just means "no history, nothing to follow yet" — we still
	// accept the socket so the client shows a live (empty) window instead of a
	// connection error.
	stream, _ := js.Stream(ctx, streamName)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeEnv := func(env envelope.Message) error {
		b, err := json.Marshal(chatView(env, me))
		if err != nil {
			return nil // skip a bad view, don't tear down
		}
		wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
		defer wcancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}

	// Reader goroutine: surfaces client disconnect (and lets us stop).
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()

	if stream != nil {
		// 1) Drain retained history, capturing the last sequence.
		lastSeq, derr := replayStream(ctx, stream, writeEnv)
		if derr != nil && ctx.Err() != nil {
			return // client went away mid-drain
		}
		// 2) Follow live from just past the drained history.
		consumer, cerr := stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
			FilterSubjects: []string{subject},
			DeliverPolicy:  jetstream.DeliverByStartSequencePolicy,
			OptStartSeq:    lastSeq + 1,
		})
		if cerr == nil {
			cc, ccerr := consumer.Consume(func(msg jetstream.Msg) {
				env, uerr := envelope.Unmarshal(msg.Data())
				if uerr != nil {
					_ = msg.Ack()
					return
				}
				if err := writeEnv(env); err != nil {
					cancel()
					return
				}
				_ = msg.Ack()
			})
			if ccerr == nil {
				defer cc.Stop()
			}
		}
	}

	<-ctx.Done()
}
