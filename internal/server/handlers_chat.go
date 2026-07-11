package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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
	// The "send as" picker: message handles the viewer created (the web analog
	// of the CLI current handle). Empty => the composer blocks with guidance.
	var handles []string
	if sources, serr := db.ListSourcesForOrg(ctx, s.Pool, org.ID); serr == nil {
		handles = ownedMessageHandles(sources, UserIDFromCtx(r.Context()))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := s.base()
	data["Org"] = org
	data["Roster"] = roster
	data["Me"] = s.meFromCtx(r.Context())
	data["Handles"] = handles
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

// chatHistoryLimit caps how many of a window's most-recent messages the
// history drain replays (tail-N). Without it a busy pipe would stream its
// entire backlog — and one GetMsg round-trip per message — on every open, the
// same degeneracy the CLI's read-flood cap already guards against. The live WS
// follow after the drain is unbounded (new messages only), so this only bounds
// the initial backlog, not the ongoing tail.
const chatHistoryLimit = 200

// chatReplayStart bounds a history drain to the most-recent `limit` messages
// (tail-N): it returns the first sequence to replay for a stream whose retained
// window is [firstSeq, lastSeq]. When that window already fits under the cap
// (or limit <= 0, meaning uncapped) it returns firstSeq unchanged; otherwise it
// returns lastSeq-limit+1. The span check guarantees that result never precedes
// firstSeq. Bounding the start caps both the bytes streamed and the number of
// per-seq GetMsg round-trips, so opening a busy window can't dump its whole
// backlog.
func chatReplayStart(firstSeq, lastSeq uint64, limit int) uint64 {
	if limit <= 0 || lastSeq < firstSeq {
		return firstSeq
	}
	if lastSeq-firstSeq+1 <= uint64(limit) {
		return firstSeq
	}
	return lastSeq - uint64(limit) + 1
}

// replayStream iterates a stream's retained messages front-to-back, invoking
// fn for each decoded envelope, and returns the last sequence it reached so a
// follow can resume from there. The drain is bounded to the most-recent `limit`
// messages (see chatReplayStart); `limit <= 0` replays the whole window. Shared
// by the JSON snapshot and the WS history replay so the two can't drift.
func replayStream(ctx context.Context, stream jetstream.Stream, limit int, fn func(envelope.Message) error) (uint64, error) {
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
	for seq := chatReplayStart(info.State.FirstSeq, info.State.LastSeq, limit); seq <= info.State.LastSeq; seq++ {
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
	_, _ = replayStream(ctx, stream, chatHistoryLimit, func(env envelope.Message) error {
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
	// `you` marks messages the viewer sent, keyed on the handle they're acting
	// as (?as=). Read-side only, so it's cosmetic — unlike send, it isn't an
	// ownership gate, so we don't validate it here.
	me := r.URL.Query().Get("as")
	js, _ := s.JSFor(ctx, org.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": drainChatMessages(ctx, js, streamName, me),
	})
}

// chatSendRequest is the POST body for a web-originated message. As is the
// handle the viewer is acting as — stamped as the envelope sender.
type chatSendRequest struct {
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Payload string `json:"payload"`
	As      string `json:"as"` // sending handle; must be a message source the viewer created
}

// handleGUIChatSend publishes a message from the browser to the resolved
// window's subject, stamped with the viewer's chosen handle (req.As) as sender.
// The handle must be a message source the session user created — enforced here
// so a crafted request can't impersonate a handle the picker never offered.
// Mirrors the scheduler's durable publish (blocks for the JetStream PubAck).
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
	if req.As == "" {
		http.Error(w, "missing sending handle", http.StatusBadRequest)
		return
	}
	// Ownership gate: you may only send AS a message handle you created. A
	// foreign or non-existent handle is a uniform 403 — we don't distinguish
	// "not yours" from "doesn't exist" so the endpoint can't be used to probe
	// which handles exist in the org.
	src, err := db.GetSourceByHandle(ctx, s.Pool, org.ID, req.As)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.Error(w, "not your handle", http.StatusForbidden)
		} else {
			http.Error(w, err.Error(), 500)
		}
		return
	}
	if !messageHandleOwnedBy(src, UserIDFromCtx(r.Context())) {
		http.Error(w, "not your handle", http.StatusForbidden)
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
	env := envelope.New(req.As, "", req.Payload, time.Now().UTC())
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
	writeJSON(w, http.StatusOK, chatView(env, req.As))
}

// chatAddPipeRequest is the POST body for creating a pipe from the console.
type chatAddPipeRequest struct {
	Name string `json:"name"`
}

// handleGUIChatAddPipe creates a root-manifold uncollared pipe (+ its stream)
// from the web console — the browser analog of the TUI's `[+ add pipe]`. Bare
// leaf only (matches the TUI affordance); collared pipes stay a CLI concern.
//
// Route: POST /orgs/{id}/chat/pipes
func (s *Server) handleGUIChatAddPipe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, ok := s.resolveChatOrg(ctx, w, r)
	if !ok {
		return
	}
	var req chatAddPipeRequest
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := natsubj.ValidateUserPipeName(req.Name); err != nil {
		http.Error(w, "invalid pipe name", http.StatusBadRequest)
		return
	}
	// First-wins collision: a source at the root manifold reserves its name.
	if exists, err := db.SourceExistsAtManifold(ctx, s.Pool, org.ID, "", req.Name); err != nil {
		http.Error(w, err.Error(), 500)
		return
	} else if exists {
		http.Error(w, "name taken by a source", http.StatusConflict)
		return
	}
	pipe, err := db.InsertPipe(ctx, s.Pool, org.ID, "", nil, UserIDFromCtx(r.Context()), req.Name, nil, nil, nil)
	if err != nil {
		if errors.Is(err, db.ErrPipeNameTaken) {
			http.Error(w, "pipe already exists", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}
	js, err := s.JSFor(ctx, org.ID)
	if err != nil {
		http.Error(w, "org account: "+err.Error(), 500)
		return
	}
	if err := ensurePipeStreamWithRetention(ctx, js, org.ID, "", "", pipe.Name,
		defaultStreamMaxAge, defaultStreamMaxMsgs, int64(defaultStreamMaxBytes)); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": pipe.Name, "target": pipe.Name})
}

// handleGUIChatRemovePipe deletes an uncollared pipe (row + stream) from the
// console — the browser analog of the TUI's `-`. target is the roster key
// ("<manifold>.<name>" or bare "<name>"). Idempotent-ish: a missing row is 404.
//
// Route: DELETE /orgs/{id}/chat/pipes?target=
func (s *Server) handleGUIChatRemovePipe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, ok := s.resolveChatOrg(ctx, w, r)
	if !ok {
		return
	}
	manifold, name := splitManifoldName(r.URL.Query().Get("target"))
	if err := natsubj.ValidatePipe(name); err != nil {
		http.Error(w, "invalid pipe name", http.StatusBadRequest)
		return
	}
	if manifold != "" {
		for _, seg := range strings.Split(manifold, ".") {
			if err := natsubj.ValidateHandle(seg); err != nil {
				http.Error(w, "invalid pipe manifold", http.StatusBadRequest)
				return
			}
		}
	}
	if err := db.DeleteUncollaredPipe(ctx, s.Pool, org.ID, manifold, name); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.Error(w, "pipe not found", 404)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}
	js, err := s.JSFor(ctx, org.ID)
	if err != nil {
		http.Error(w, "org account: "+err.Error(), 500)
		return
	}
	if err := deleteUncollaredPipeStream(ctx, js, org.ID, manifold, name); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	me := r.URL.Query().Get("as") // acting handle, for cosmetic `you` labeling
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
		// 1) Drain retained history (bounded to the most-recent
		// chatHistoryLimit), capturing the last sequence.
		lastSeq, derr := replayStream(ctx, stream, chatHistoryLimit, writeEnv)
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
