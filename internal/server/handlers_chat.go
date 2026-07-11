package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

// streamLastSeq returns a stream's newest sequence (0 if the stream is missing
// or the org is unprovisioned). One Info() round-trip — cheap enough to fan out
// across the roster.
func streamLastSeq(ctx context.Context, js jetstream.JetStream, name string) int64 {
	if js == nil {
		return 0
	}
	stream, err := js.Stream(ctx, name)
	if err != nil {
		return 0
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return 0
	}
	return int64(info.State.LastSeq)
}

// countAfter counts how many of seqs are strictly past cursor — the unread
// tail of a DM once the per-conversation read position is applied.
func countAfter(seqs []uint64, cursor int64) int {
	n := 0
	for _, s := range seqs {
		if int64(s) > cursor {
			n++
		}
	}
	return n
}

// drainInboxBySender reads the acting handle's inbox (bounded) and buckets each
// message's stream sequence by sender — the raw material for per-counterparty
// DM unread. Missing stream => empty.
func drainInboxBySender(ctx context.Context, js jetstream.JetStream, streamName string) map[string][]uint64 {
	out := map[string][]uint64{}
	if js == nil {
		return out
	}
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		return out
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs == 0 {
		return out
	}
	for seq := chatReplayStart(info.State.FirstSeq, info.State.LastSeq, chatHistoryLimit); seq <= info.State.LastSeq; seq++ {
		msg, gerr := stream.GetMsg(ctx, seq)
		if gerr != nil {
			continue
		}
		env, uerr := envelope.Unmarshal(msg.Data)
		if uerr != nil {
			continue
		}
		out[env.Sender] = append(out[env.Sender], seq)
	}
	return out
}

// stampUnread fills each roster row's Unread from the viewer's read cursors.
//
//   - Pipes (shared rooms): unread = max(0, pipe.LastSeq - cursor).
//   - Source windows (DMs), acting as handle H: the counterparty's messages to
//     me land in H.inbox, so unread = how many of H.inbox's messages from that
//     counterparty are past this conversation's cursor. H.inbox is drained once
//     and bucketed by sender. With no acting handle (god's-eye), a source falls
//     back to its own-inbox growth.
//
// Cursors load in one query; pipe LastSeq reads fan out concurrently. Best-
// effort — a cursor/JS failure yields no badges rather than a failed page.
func (s *Server) stampUnread(ctx context.Context, org db.Account, userID uuid.UUID, acting string, roster chatRoster) chatRoster {
	if userID == uuid.Nil {
		return roster
	}
	cursors, err := db.ListChatReadCursors(ctx, s.Pool, org.ID, userID)
	if err != nil {
		return roster
	}
	js, _ := s.JSFor(ctx, org.ID)
	if js == nil {
		return roster
	}

	var wg sync.WaitGroup
	// Pipes: own-stream growth past the cursor. Keyed by the acting handle so
	// read state is per-identity (each handle is its own participant, like the
	// TUI's per-handle chatstore) — reading a pipe as one handle doesn't mark it
	// read for another.
	for i := range roster.Pipes {
		streamName := natsubj.BuildStreamName(org.ID, roster.Pipes[i].Namespace, "", roster.Pipes[i].Label)
		cursor := cursors[db.ChatCursorKey("pipe", acting, roster.Pipes[i].Target)]
		wg.Add(1)
		go func(i int, streamName string, cursor int64) {
			defer wg.Done()
			roster.Pipes[i].Unread = unreadCount(streamLastSeq(ctx, js, streamName), cursor)
		}(i, streamName, cursor)
	}

	// Source windows (agents+inboxes) = DMs.
	if acting != "" {
		hManifold := ""
		if h, herr := db.GetSourceByHandle(ctx, s.Pool, org.ID, acting); herr == nil {
			hManifold = h.Manifold
		}
		bySender := drainInboxBySender(ctx, js, natsubj.BuildStreamName(org.ID, hManifold, acting, "inbox"))
		mark := func(entries []chatEntry) {
			for i := range entries {
				cursor := cursors[db.ChatCursorKey("source", acting, entries[i].Target)]
				entries[i].Unread = countAfter(bySender[entries[i].Target], cursor)
			}
		}
		mark(roster.Agents)
		mark(roster.Inboxes)
	} else {
		// God's-eye fallback: a source's own-inbox growth.
		stampOwn := func(entries []chatEntry) {
			for i := range entries {
				streamName := natsubj.BuildStreamName(org.ID, entries[i].Namespace, entries[i].Target, "inbox")
				cursor := cursors[db.ChatCursorKey("source", "", entries[i].Target)]
				wg.Add(1)
				go func(i int, entries []chatEntry, streamName string, cursor int64) {
					defer wg.Done()
					entries[i].Unread = unreadCount(streamLastSeq(ctx, js, streamName), cursor)
				}(i, entries, streamName, cursor)
			}
		}
		stampOwn(roster.Agents)
		stampOwn(roster.Inboxes)
	}
	wg.Wait()
	return roster
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
	// The viewer's owned handles feed the top-bar identity switcher; the acting
	// handle (?as=, defaulting to the first owned) is who they are for this
	// view. Scope the roster to that identity — drop self so you can't DM
	// yourself — then stamp unread on what remains.
	var handles []string
	if sources, serr := db.ListSourcesForOrg(ctx, s.Pool, org.ID); serr == nil {
		handles = ownedMessageHandles(sources, UserIDFromCtx(r.Context()))
	}
	acting := pickActingHandle(r.URL.Query().Get("as"), handles)
	roster = roster.excludeHandle(acting)
	roster = s.stampUnread(ctx, org, UserIDFromCtx(r.Context()), acting, roster)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := s.base()
	data["Org"] = org
	data["Roster"] = roster
	data["Me"] = s.meFromCtx(r.Context())
	data["Handles"] = handles
	data["ActingHandle"] = acting
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

// maxChatPayload caps a web-originated message's length (runes), matching the
// composer's client-side maxlength so a direct POST can't publish something
// arbitrarily large.
const maxChatPayload = 2000

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

// chatReadLeg is one stream to read for a window, optionally filtered to a
// single sender. A pipe (shared room) or a god's-eye inbox is a single
// unfiltered leg; a DM thread is two filtered legs (my side + the
// counterparty's side).
type chatReadLeg struct {
	StreamName string
	WantSender string // "" = no sender filter
}

// resolveChatReadLegs validates a (kind,target) window and returns the legs
// whose merged, sender-filtered messages are what the viewer should see.
//
// For a source (DM) window opened while acting as handle `as` (≠ the target),
// that's the two-way thread — TUI participant parity: <target>.inbox filtered
// to my sends, stitched with <as>.inbox filtered to the counterparty's replies.
// With no acting handle (or acting-as the target itself) it degrades to the
// single unfiltered inbox — the god's-eye view. Pipes are always one unfiltered
// leg. Writes the HTTP error + returns false on failure.
func (s *Server) resolveChatReadLegs(ctx context.Context, w http.ResponseWriter, org db.Account, kind, target, as string) ([]chatReadLeg, bool) {
	win, err := resolveChatWindow(org.ID, kind, target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if win.Kind == "pipe" {
		exists, err := db.UncollaredPipeExists(ctx, s.Pool, org.ID, win.Manifold, win.Name)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return nil, false
		}
		if !exists {
			http.Error(w, "pipe not found", 404)
			return nil, false
		}
		return []chatReadLeg{{StreamName: win.StreamName}}, true
	}
	// source window
	x, err := db.GetSourceByHandle(ctx, s.Pool, org.ID, win.Handle)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.Error(w, "source not found", 404)
		} else {
			http.Error(w, err.Error(), 500)
		}
		return nil, false
	}
	xStream := natsubj.BuildStreamName(org.ID, x.Manifold, x.Handle, "inbox")
	if as == "" || as == x.Handle {
		return []chatReadLeg{{StreamName: xStream}}, true // god's-eye / own inbox
	}
	// Resolve the acting handle's manifold (best-effort; root if unknown) to
	// build its inbox stream — that's where the counterparty's replies land.
	hManifold := ""
	if h, herr := db.GetSourceByHandle(ctx, s.Pool, org.ID, as); herr == nil {
		hManifold = h.Manifold
	}
	hStream := natsubj.BuildStreamName(org.ID, hManifold, as, "inbox")
	return []chatReadLeg{
		{StreamName: xStream, WantSender: as},       // my messages to the counterparty
		{StreamName: hStream, WantSender: x.Handle}, // the counterparty's replies to me
	}, true
}

// drainChatThread reads every leg, applies each leg's sender filter, merges by
// created_at and dedups by id — the snapshot form of a window (a DM thread's
// two legs, or a single pipe/inbox stream). Missing streams are skipped.
func drainChatThread(ctx context.Context, js jetstream.JetStream, legs []chatReadLeg, me string) []chatMessageView {
	out := []chatMessageView{}
	if js == nil {
		return out
	}
	seen := map[string]bool{}
	for _, leg := range legs {
		stream, err := js.Stream(ctx, leg.StreamName)
		if err != nil {
			continue
		}
		wantSender := leg.WantSender
		_, _ = replayStream(ctx, stream, chatHistoryLimit, func(env envelope.Message) error {
			if wantSender != "" && env.Sender != wantSender {
				return nil
			}
			if env.ID != "" {
				if seen[env.ID] {
					return nil
				}
				seen[env.ID] = true
			}
			out = append(out, chatView(env, me))
			return nil
		})
	}
	// created_at is RFC3339-UTC to the second, so a lexical sort is chronological;
	// stable keeps same-second messages in leg order.
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
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
	// `as` is the handle the viewer is acting as: it selects the DM thread's
	// counterparty legs and keys the cosmetic `you` label. Read-side only, not
	// an ownership gate (unlike send), so it isn't validated here.
	me := r.URL.Query().Get("as")
	legs, ok := s.resolveChatReadLegs(ctx, w, org, kind, target, me)
	if !ok {
		return
	}
	js, _ := s.JSFor(ctx, org.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": drainChatThread(ctx, js, legs, me),
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
	// Server-side length cap (the composer's maxlength is client-only). Bounds
	// a direct POST from publishing an arbitrarily large message.
	if utf8.RuneCountInString(req.Payload) > maxChatPayload {
		http.Error(w, "payload too long", http.StatusBadRequest)
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

// handleGUIChatRoster returns the three-section roster as JSON so the browser
// can re-poll agent liveness (dots/state) and the top-bar counts without a
// full page reload — the web analog of the TUI's periodic `who` poll.
//
// Route: GET /orgs/{id}/chat/roster
func (s *Server) handleGUIChatRoster(w http.ResponseWriter, r *http.Request) {
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
	// Scope to the acting identity the client is polling as (?as=), same as the
	// page render, so a live refresh doesn't re-introduce the self row.
	acting := r.URL.Query().Get("as")
	roster = roster.excludeHandle(acting)
	roster = s.stampUnread(ctx, org, UserIDFromCtx(r.Context()), acting, roster)
	writeJSON(w, http.StatusOK, map[string]any{
		"agents":  roster.Agents,
		"inboxes": roster.Inboxes,
		"pipes":   roster.Pipes,
		"online":  roster.OnlineCount(),
	})
}

// chatReadRequest is the POST body for advancing a conversation's read cursor.
// As is the handle the viewer is acting as (their identity for this DM).
type chatReadRequest struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	As     string `json:"as"`
}

// handleGUIChatMarkRead advances the viewer's read cursor for one conversation,
// clearing its unread badge. For a DM (source), "read up to now" means my inbox's
// current newest sequence — that's the stream the counterparty's messages arrive
// on. For a pipe it's the pipe stream. Called when a window is opened (and
// periodically while it stays open).
//
// Route: POST /orgs/{id}/chat/read
func (s *Server) handleGUIChatMarkRead(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, ok := s.resolveChatOrg(ctx, w, r)
	if !ok {
		return
	}
	var req chatReadRequest
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	win, err := resolveChatWindow(org.ID, req.Kind, req.Target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	js, _ := s.JSFor(ctx, org.ID)

	var streamName, acting string
	if win.Kind == "pipe" {
		streamName = win.StreamName
		acting = req.As // per-identity pipe read state
	} else {
		// DM: mark against MY inbox (acting handle), where the counterparty's
		// messages land. No acting handle => the window's own inbox (god's-eye).
		acting = req.As
		h := acting
		if h == "" {
			h = win.Handle
		}
		hManifold := ""
		if src, e := db.GetSourceByHandle(ctx, s.Pool, org.ID, h); e == nil {
			hManifold = src.Manifold
		}
		streamName = natsubj.BuildStreamName(org.ID, hManifold, h, "inbox")
	}
	last := streamLastSeq(ctx, js, streamName)
	if err := db.UpsertChatReadCursor(ctx, s.Pool, org.ID, UserIDFromCtx(r.Context()), req.Kind, acting, req.Target, last); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unread": 0, "seq": last})
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
	target := r.URL.Query().Get("target")
	manifold, name := splitManifoldName(target)
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
	// Clear read cursors so a same-name recreate doesn't inherit a stale
	// last_read_seq (best-effort; a leftover cursor only affects badges).
	_ = db.DeleteChatReadCursorsForTarget(ctx, s.Pool, org.ID, "pipe", target)
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
	me := r.URL.Query().Get("as") // acting handle: selects DM legs + cosmetic `you`
	legs, ok := s.resolveChatReadLegs(ctx, w, org, kind, target, me)
	if !ok {
		return
	}
	js, err := s.JSFor(ctx, org.ID)
	if err != nil {
		http.Error(w, "org account: "+err.Error(), 500)
		return
	}

	// nil opts => coder/websocket's default same-origin check: a cross-site
	// browser handshake (Origin host != request Host) is rejected 403 — CSWSH
	// defense-in-depth — while non-browser clients (no Origin) are accepted.
	// Keying off the request Host (not a fixed BaseURL) keeps localhost, the
	// prod domain, and Host-forwarding proxies all working. (Session cookies are
	// SameSite=Lax, so the ambient-cookie vector is already mitigated; this is
	// belt-and-braces.)
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Serialize writes: a DM thread follows two legs (two consumer goroutines),
	// and coder/websocket forbids concurrent writes on one connection.
	var writeMu sync.Mutex
	writeView := func(v chatMessageView) error {
		b, err := json.Marshal(v)
		if err != nil {
			return nil // skip a bad view, don't tear down
		}
		writeMu.Lock()
		defer writeMu.Unlock()
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

	// 1) Drain each leg's retained history (bounded + sender-filtered), merge
	//    by created_at, and send once. Record each leg's last sequence so the
	//    live follow resumes from just past it. A leg whose stream isn't
	//    materialised yet is simply skipped (empty history, nothing to follow).
	type legState struct {
		stream     jetstream.Stream
		wantSender string
		lastSeq    uint64
	}
	var states []legState
	var hist []chatMessageView
	for _, leg := range legs {
		stream, serr := js.Stream(ctx, leg.StreamName)
		if serr != nil {
			continue
		}
		wantSender := leg.WantSender
		last, derr := replayStream(ctx, stream, chatHistoryLimit, func(env envelope.Message) error {
			if wantSender == "" || env.Sender == wantSender {
				hist = append(hist, chatView(env, me))
			}
			return nil
		})
		if derr != nil && ctx.Err() != nil {
			return // client went away mid-drain
		}
		states = append(states, legState{stream: stream, wantSender: wantSender, lastSeq: last})
	}
	sort.SliceStable(hist, func(i, j int) bool { return hist[i].CreatedAt < hist[j].CreatedAt })
	for _, v := range hist {
		if err := writeView(v); err != nil {
			return
		}
	}

	// 2) Follow each leg live from just past its drained history. Per-leg
	//    consumers deliver in arrival order (≈ time order for live traffic);
	//    the browser dedups by id so any overlap is harmless.
	for _, st := range states {
		wantSender := st.wantSender
		consumer, cerr := st.stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
			DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
			OptStartSeq:   st.lastSeq + 1,
		})
		if cerr != nil {
			continue
		}
		cc, ccerr := consumer.Consume(func(msg jetstream.Msg) {
			env, uerr := envelope.Unmarshal(msg.Data())
			if uerr != nil {
				_ = msg.Ack()
				return
			}
			if wantSender != "" && env.Sender != wantSender {
				_ = msg.Ack()
				return
			}
			if err := writeView(chatView(env, me)); err != nil {
				cancel()
				return
			}
			_ = msg.Ack()
		})
		if ccerr == nil {
			defer cc.Stop()
		}
	}

	<-ctx.Done()
}
