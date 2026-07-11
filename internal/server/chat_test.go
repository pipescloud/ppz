package server

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// classifyAgentStatus must reproduce the daemon's ClassifyHeartbeatStatus
// thresholds so the web roster's status dots agree with `ppz who`:
//   - age < 1.5×interval          -> online
//   - 1.5×interval ≤ age < 3×      -> stale
//   - age ≥ 3×interval / no beat   -> offline
//   - future-dated beat (skew)     -> online
func TestClassifyAgentStatus(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	const iv = 60
	for _, tc := range []struct {
		name string
		last time.Time
		iv   int
		want string
	}{
		{"fresh", now.Add(-10 * time.Second), iv, "online"},
		{"just-under-1.5x", now.Add(-89 * time.Second), iv, "online"},
		{"at-2x-is-stale", now.Add(-120 * time.Second), iv, "stale"},
		{"just-under-3x", now.Add(-179 * time.Second), iv, "stale"},
		{"at-3x-is-offline", now.Add(-180 * time.Second), iv, "offline"},
		{"way-old", now.Add(-1 * time.Hour), iv, "offline"},
		{"no-beat", time.Time{}, iv, "offline"},
		{"future-skew", now.Add(30 * time.Second), iv, "online"},
		{"zero-interval-defaults-60", now.Add(-10 * time.Second), 0, "online"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAgentStatus(tc.last, now, tc.iv); got != tc.want {
				t.Errorf("classifyAgentStatus(age=%v, iv=%d) = %q, want %q",
					now.Sub(tc.last), tc.iv, got, tc.want)
			}
		})
	}
}

func ptySource(handle, manifold string) db.Source {
	return db.Source{ID: uuid.New(), Handle: handle, Manifold: manifold, Kind: db.SourceKindPTY}
}
func msgSource(handle, manifold string) db.Source {
	return db.Source{ID: uuid.New(), Handle: handle, Manifold: manifold, Kind: db.SourceKindMessage}
}

// buildChatRoster splits sources into AGENTS (pty) and INBOXES (message),
// lists uncollared pipes as PIPES, and stamps agent liveness from the
// heartbeat time. Mirrors the TUI's three roster sections.
func TestBuildChatRoster_ClassifiesAndSorts(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	sources := []chatSourceInput{
		{Source: ptySource("zeta", ""), HeartbeatAt: now.Add(-5 * time.Second), IntervalSec: 60, AgentState: "working"},
		{Source: msgSource("bot", "")},
		{Source: ptySource("alpha", "team"), HeartbeatAt: time.Time{}},
		{Source: msgSource("aardvark", "")},
	}
	pipes := []chatPipeInput{
		{Manifold: "", Name: "general"},
		{Manifold: "eng", Name: "backend"},
	}

	r := buildChatRoster(sources, pipes, now)

	// AGENTS: pty only, sorted by handle.
	if len(r.Agents) != 2 {
		t.Fatalf("Agents = %d, want 2 (%+v)", len(r.Agents), r.Agents)
	}
	if r.Agents[0].Target != "alpha" || r.Agents[1].Target != "zeta" {
		t.Errorf("agents not sorted by handle: %q, %q", r.Agents[0].Target, r.Agents[1].Target)
	}
	if r.Agents[0].Kind != chatKindAgent || !r.Agents[0].HasStatus {
		t.Errorf("agent[0] wrong kind/hasstatus: %+v", r.Agents[0])
	}
	// alpha has no heartbeat -> offline; zeta fresh -> online|working.
	if r.Agents[0].Status != "offline" {
		t.Errorf("alpha status = %q, want offline", r.Agents[0].Status)
	}
	if r.Agents[1].Status != "online" || r.Agents[1].State != "working" {
		t.Errorf("zeta = %q/%q, want online/working", r.Agents[1].Status, r.Agents[1].State)
	}

	// INBOXES: message-kind only, sorted by handle, no status dot.
	if len(r.Inboxes) != 2 {
		t.Fatalf("Inboxes = %d, want 2", len(r.Inboxes))
	}
	if r.Inboxes[0].Target != "aardvark" || r.Inboxes[1].Target != "bot" {
		t.Errorf("inboxes not sorted: %q, %q", r.Inboxes[0].Target, r.Inboxes[1].Target)
	}
	if r.Inboxes[0].Kind != chatKindInbox || r.Inboxes[0].HasStatus {
		t.Errorf("inbox[0] wrong kind/hasstatus: %+v", r.Inboxes[0])
	}

	// PIPES: dotted target, leaf label, sorted by (manifold,name).
	if len(r.Pipes) != 2 {
		t.Fatalf("Pipes = %d, want 2", len(r.Pipes))
	}
	// "" < "eng" so general(root) sorts before eng.backend.
	if r.Pipes[0].Target != "general" || r.Pipes[0].Label != "general" {
		t.Errorf("pipe[0] = %+v, want target/label general", r.Pipes[0])
	}
	if r.Pipes[1].Target != "eng.backend" || r.Pipes[1].Label != "backend" {
		t.Errorf("pipe[1] = %+v, want target eng.backend label backend", r.Pipes[1])
	}
	if r.Pipes[1].Kind != chatKindPipe {
		t.Errorf("pipe kind = %q", r.Pipes[1].Kind)
	}
}

// The web picks an acting handle up front (top-bar identity), defaulting to the
// viewer's first owned handle when the request doesn't name a valid one — you
// can only ever act as a handle you own.
func TestPickActingHandle(t *testing.T) {
	owned := []string{"desk", "ops"}
	for _, tc := range []struct {
		name, requested, want string
		owned                 []string
	}{
		{"honours-valid-request", "ops", "ops", owned},
		{"defaults-to-first", "", "desk", owned},
		{"unknown-falls-back-to-first", "nope", "desk", owned},
		{"none-owned", "ops", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickActingHandle(tc.requested, tc.owned); got != tc.want {
				t.Errorf("pickActingHandle(%q, %v) = %q, want %q", tc.requested, tc.owned, got, tc.want)
			}
		})
	}
}

// The roster is scoped to the acting identity: your own handle is dropped from
// the DM-able sections (agents/inboxes) so you can't DM yourself — matching the
// TUI's `Handle == m.me` exclusion. Pipes (shared rooms) are untouched.
func TestChatRoster_ExcludeHandle(t *testing.T) {
	r := chatRoster{
		Agents:  []chatEntry{{Target: "botty"}},
		Inboxes: []chatEntry{{Target: "desk"}, {Target: "ops"}},
		Pipes:   []chatEntry{{Target: "general"}, {Target: "desk"}},
	}
	got := r.excludeHandle("desk")
	if len(got.Inboxes) != 1 || got.Inboxes[0].Target != "ops" {
		t.Errorf("inboxes = %+v, want only ops", got.Inboxes)
	}
	if len(got.Agents) != 1 {
		t.Errorf("agents changed unexpectedly: %+v", got.Agents)
	}
	// A pipe that happens to share the name is NOT dropped — pipes aren't DMs.
	if len(got.Pipes) != 2 {
		t.Errorf("pipes should be untouched, got %+v", got.Pipes)
	}
}

// Unread badges: a window's unread count is the number of stream sequences
// past the viewer's read cursor — max(0, lastSeq - cursorSeq), clamped so a
// cursor somehow ahead of the stream never goes negative.
func TestUnreadCount(t *testing.T) {
	for _, tc := range []struct {
		name             string
		last, cursor     int64
		want             int
	}{
		{"fresh-behind", 5, 2, 3},
		{"caught-up", 2, 2, 0},
		{"cursor-ahead-clamps", 2, 5, 0},
		{"empty-stream", 0, 0, 0},
		{"never-read", 4, 0, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unreadCount(tc.last, tc.cursor); got != tc.want {
				t.Errorf("unreadCount(%d,%d) = %d, want %d", tc.last, tc.cursor, got, tc.want)
			}
		})
	}
}

// A web user acts AS one of their own message-kind handles (the analog of the
// CLI's `ppz set handle`): the picker lists only sources the session user
// created, and send re-validates ownership server-side. ownedMessageHandles is
// the pure filter behind both — message-kind + created_by == me, sorted.
func TestOwnedMessageHandles(t *testing.T) {
	me := uuid.New()
	other := uuid.New()
	msg := func(h string, owner uuid.UUID) db.Source {
		return db.Source{ID: uuid.New(), Handle: h, Kind: db.SourceKindMessage, CreatedByUserID: owner}
	}
	pty := func(h string, owner uuid.UUID) db.Source {
		return db.Source{ID: uuid.New(), Handle: h, Kind: db.SourceKindPTY, CreatedByUserID: owner}
	}
	sources := []db.Source{
		msg("zeta", me),      // owned message -> included
		msg("alpha", me),     // owned message -> included (sorts first)
		msg("foreign", other), // someone else's message -> excluded
		pty("botty", me),     // my pty/agent -> excluded (can't act as an agent)
	}
	got := ownedMessageHandles(sources, me)
	want := []string{"alpha", "zeta"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ownedMessageHandles = %v, want %v", got, want)
	}

	// The predicate the send path re-checks: only my own message handle passes.
	if !messageHandleOwnedBy(msg("desk", me), me) {
		t.Error("own message handle should be actable")
	}
	if messageHandleOwnedBy(msg("desk", other), me) {
		t.Error("someone else's handle must NOT be actable")
	}
	if messageHandleOwnedBy(pty("agent", me), me) {
		t.Error("a pty/agent handle must NOT be actable (message-kind only)")
	}
}

// The web chat pane header must match the TUI's tChatTitle byte-for-byte so
// the two consoles read identically:
//   - agent:  "<label> · dm · <status>"  (+ "|<state>" when an agent_state is set)
//   - inbox:  "<label> · dm · inbox"
//   - pipe:   "#<label> · pipe (uncollared)"
// buildChatRoster stamps Title on each entry (server-side, so it's testable
// here rather than living only in the browser JS).
func TestBuildChatRoster_TitleParity(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	sources := []chatSourceInput{
		{Source: ptySource("claude", ""), HeartbeatAt: now.Add(-5 * time.Second), IntervalSec: 60, AgentState: "working"},
		{Source: ptySource("codex", ""), HeartbeatAt: time.Time{}}, // no beat -> offline, no state
		{Source: msgSource("ops", "")},
	}
	pipes := []chatPipeInput{
		{Manifold: "", Name: "general"},
		{Manifold: "eng", Name: "backend"},
	}
	r := buildChatRoster(sources, pipes, now)

	// agents sorted: claude, codex
	if got := r.Agents[0].Title; got != "claude · dm · online|working" {
		t.Errorf("agent title = %q, want %q", got, "claude · dm · online|working")
	}
	if got := r.Agents[1].Title; got != "codex · dm · offline" {
		t.Errorf("agent(no state) title = %q, want %q", got, "codex · dm · offline")
	}
	if got := r.Inboxes[0].Title; got != "ops · dm · inbox" {
		t.Errorf("inbox title = %q, want %q", got, "ops · dm · inbox")
	}
	// pipes sorted: general(root), then eng.backend
	if got := r.Pipes[0].Title; got != "#general · pipe (uncollared)" {
		t.Errorf("pipe(root) title = %q, want %q", got, "#general · pipe (uncollared)")
	}
	if got := r.Pipes[1].Title; got != "#backend · pipe (uncollared)" {
		t.Errorf("pipe(manifolded) title = %q, want %q", got, "#backend · pipe (uncollared)")
	}
}

// OnlineCount powers the top-bar "<N> online · <N> agents · <N> pipes" summary,
// matching the TUI titleBar. Only agents with status "online" count.
func TestChatRoster_OnlineCount(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	r := buildChatRoster([]chatSourceInput{
		{Source: ptySource("a", ""), HeartbeatAt: now.Add(-5 * time.Second), IntervalSec: 60},
		{Source: ptySource("b", ""), HeartbeatAt: now.Add(-5 * time.Minute), IntervalSec: 60}, // offline
		{Source: ptySource("c", ""), HeartbeatAt: now.Add(-2 * time.Second), IntervalSec: 60},
	}, nil, now)
	if got := r.OnlineCount(); got != 2 {
		t.Errorf("OnlineCount() = %d, want 2", got)
	}
}

// resolveChatWindow validates a (kind,target) pair and, for pipe windows,
// builds the JetStream subject + stream name (source windows are resolved
// against the DB row by resolveChatWindowDB, so this pure resolver only
// validates their handle). Malformed targets are rejected so a guessed URL
// can't stream an arbitrary subject.
func TestResolveChatWindow(t *testing.T) {
	acct := uuid.New()

	// Pipe windows carry the manifold in the target, so subject/stream are
	// fully resolved by the pure function.
	for _, tc := range []struct {
		name       string
		target     string
		wantSubj   string
		wantStream string
	}{
		{
			name:       "pipe-root",
			target:     "general",
			wantSubj:   natsubj.BuildSubject(acct, "", "", "general"),
			wantStream: natsubj.BuildStreamName(acct, "", "", "general"),
		},
		{
			name:       "pipe-manifolded",
			target:     "eng.backend",
			wantSubj:   natsubj.BuildSubject(acct, "eng", "", "backend"),
			wantStream: natsubj.BuildStreamName(acct, "eng", "", "backend"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, err := resolveChatWindow(acct, "pipe", tc.target)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if w.Subject != tc.wantSubj {
				t.Errorf("Subject = %q, want %q", w.Subject, tc.wantSubj)
			}
			if w.StreamName != tc.wantStream {
				t.Errorf("StreamName = %q, want %q", w.StreamName, tc.wantStream)
			}
		})
	}

	// Source windows validate the handle and defer subject building to the
	// DB layer (Subject/StreamName stay empty here).
	t.Run("source-validates-only", func(t *testing.T) {
		w, err := resolveChatWindow(acct, "source", "alice")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w.Handle != "alice" || w.Name != "inbox" {
			t.Errorf("source window = %+v, want handle=alice name=inbox", w)
		}
		if w.Subject != "" || w.StreamName != "" {
			t.Errorf("source Subject/StreamName should be empty (DB owns them), got %q/%q", w.Subject, w.StreamName)
		}
	})

	for _, tc := range []struct{ name, kind, target string }{
		{"unknown-kind", "bogus", "x"},
		{"empty-target", "source", ""},
		{"bad-handle", "source", "Bad Handle!"},
		{"bad-pipe-name", "pipe", "Nope!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveChatWindow(acct, tc.kind, tc.target); err == nil {
				t.Fatalf("expected error for kind=%q target=%q", tc.kind, tc.target)
			}
		})
	}
}

// chatReplayStart bounds the history drain to the most-recent N messages
// (tail-N), so a busy window doesn't stream (and GetMsg-round-trip) its entire
// backlog every time it's opened. It maps a stream's [FirstSeq, LastSeq] window
// to the first sequence to replay: the whole range when it fits under the cap,
// otherwise LastSeq-limit+1 (clamped to FirstSeq). limit <= 0 means uncapped.
func TestChatReplayStart(t *testing.T) {
	for _, tc := range []struct {
		name              string
		first, last, want uint64
		limit             int
	}{
		{"uncapped-zero", 1, 1000, 1, 0},
		{"uncapped-negative", 1, 1000, 1, -5},
		{"under-cap", 1, 50, 1, 200},
		{"exactly-at-cap", 1, 200, 1, 200},
		{"over-cap-from-one", 1, 1000, 801, 200},
		{"over-cap-nonzero-first", 500, 1000, 801, 200},
		{"under-cap-nonzero-first", 900, 1000, 900, 200},
		{"single-message", 7, 7, 7, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatReplayStart(tc.first, tc.last, tc.limit); got != tc.want {
				t.Errorf("chatReplayStart(first=%d, last=%d, limit=%d) = %d, want %d",
					tc.first, tc.last, tc.limit, got, tc.want)
			}
		})
	}
}
