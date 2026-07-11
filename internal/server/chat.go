package server

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// The web chat console is the browser port of the `ppz chat` TUI. It shows
// the same three roster sections — AGENTS (pty sources), INBOXES (message
// sources), PIPES (uncollared shared rooms) — and a chat pane that follows
// one JetStream stream per window and posts to one subject:
//
//	agent / inbox window  ->  <handle>.inbox
//	pipe window           ->  <manifold>.<name>   (uncollared)
//
// Unlike the TUI (a participant that stitches its own inbox with outbound
// echoes), the server has god's-eye JetStream access, so a window is just a
// direct follow of the target stream: our own posts come back through the
// follow, so there's no optimistic-echo / rollback / chatstore machinery to
// carry — JetStream itself is the durable record.

// chatEntryKind tags a roster row for display + backend routing.
type chatEntryKind string

const (
	chatKindAgent chatEntryKind = "agent" // pty source: live terminal/harness, heartbeats
	chatKindInbox chatEntryKind = "inbox" // message source: a bare inbox (human/service)
	chatKindPipe  chatEntryKind = "pipe"  // uncollared pipe: shared room
)

// chatEntry is one row in the roster. JSON tags feed the /chat/roster live
// refresh (the browser re-polls dots/state/counts without a full reload).
type chatEntry struct {
	Kind      chatEntryKind `json:"kind"`
	Target    string        `json:"target"`     // handle (agent/inbox) or dotted "<manifold>.<name>" (pipe) — the URL/window key
	Label     string        `json:"label"`      // display text: handle for agents/inboxes, leaf name for pipes
	Namespace string        `json:"namespace"`  // manifold ("" = root); shown as a secondary label
	Status    string        `json:"status"`     // "online" | "stale" | "offline" for agents; "" otherwise
	State     string        `json:"state"`      // agent_state (idle/working/blocked) for agents; "" otherwise
	HasStatus bool          `json:"has_status"` // agents render a live status dot; inboxes/pipes don't
	Title     string        `json:"title"`      // chat-pane header text; parity with the TUI's tChatTitle
	Unread    int           `json:"unread"`     // messages past the viewer's read cursor; 0 = none / caught up
}

// unreadCount is the number of stream sequences past the viewer's read cursor —
// the badge on a roster row. Clamped at 0 so a cursor somehow ahead of the
// stream (or an empty stream) never shows a negative count.
func unreadCount(lastSeq, cursorSeq int64) int {
	if lastSeq <= cursorSeq {
		return 0
	}
	return int(lastSeq - cursorSeq)
}

// chatTitle reproduces the TUI's tChatTitle (internal/cli/tui.go) so the web
// chat-pane header reads identically:
//
//	agent:  "<label> · dm · <status>"   (+ "|<state>" when agent_state is set)
//	inbox:  "<label> · dm · inbox"
//	pipe:   "#<label> · pipe (uncollared)"
//
// A parity test (TestBuildChatRoster_TitleParity) pins the format.
func chatTitle(kind chatEntryKind, label, status, state string) string {
	switch kind {
	case chatKindAgent:
		st := status
		if st == "" {
			st = "—"
		}
		if state != "" {
			st += "|" + state
		}
		return fmt.Sprintf("%s · dm · %s", label, st)
	case chatKindInbox:
		return fmt.Sprintf("%s · dm · inbox", label)
	default: // pipe
		return fmt.Sprintf("#%s · pipe (uncollared)", label)
	}
}

// messageHandleOwnedBy reports whether userID may act AS this source — i.e.
// stamp its handle as the sender on a web-originated send. Only message-kind
// sources the user created qualify: it's the web analog of the CLI's current
// handle (`ppz set handle` claims a bare message handle), and you can't
// impersonate a pty/agent or someone else's inbox. The send path re-checks
// this server-side so a crafted request can't spoof a handle the picker never
// offered.
func messageHandleOwnedBy(src db.Source, userID uuid.UUID) bool {
	return src.Kind == db.SourceKindMessage && src.CreatedByUserID == userID
}

// ownedMessageHandles returns, sorted, the bare handles the user may send as —
// the "send as" picker's contents. (Future no-auth mode, with no session user,
// would instead offer every handle; not reachable while every route is
// session-gated.)
func ownedMessageHandles(sources []db.Source, userID uuid.UUID) []string {
	var hs []string
	for _, s := range sources {
		if messageHandleOwnedBy(s, userID) {
			hs = append(hs, s.Handle)
		}
	}
	sort.Strings(hs)
	return hs
}

// pickActingHandle chooses the identity the viewer acts as: the requested
// handle when they own it, else their first owned handle (the default on entry),
// else "" when they own none. You can only ever act as a handle you own.
func pickActingHandle(requested string, owned []string) string {
	for _, h := range owned {
		if h == requested {
			return requested
		}
	}
	if len(owned) > 0 {
		return owned[0]
	}
	return ""
}

// excludeHandle drops the viewer's own handle from the DM-able sections
// (agents/inboxes) so they can't DM themselves — the TUI's `Handle == m.me`
// exclusion. Pipes are shared rooms and untouched.
func (r chatRoster) excludeHandle(handle string) chatRoster {
	if handle == "" {
		return r
	}
	drop := func(es []chatEntry) []chatEntry {
		out := make([]chatEntry, 0, len(es))
		for _, e := range es {
			if e.Target != handle {
				out = append(out, e)
			}
		}
		return out
	}
	r.Agents = drop(r.Agents)
	r.Inboxes = drop(r.Inboxes)
	return r
}

// OnlineCount is the number of agents currently classified "online" — powers
// the top-bar "<N> online · <N> agents · <N> pipes" summary (TUI titleBar).
func (r chatRoster) OnlineCount() int {
	n := 0
	for _, a := range r.Agents {
		if a.Status == "online" {
			n++
		}
	}
	return n
}

// chatRoster is the three-section menu.
type chatRoster struct {
	Agents  []chatEntry
	Inboxes []chatEntry
	Pipes   []chatEntry
}

// chatSourceInput is a source plus the liveness facts read from its
// heartbeat stream (zero HeartbeatAt = never beaten).
type chatSourceInput struct {
	Source      db.Source
	HeartbeatAt time.Time
	IntervalSec int
	AgentState  string
}

// chatPipeInput is one uncollared pipe.
type chatPipeInput struct {
	Manifold string
	Name     string
}

// classifyAgentStatus reproduces the daemon's ClassifyHeartbeatStatus
// (internal/daemon/heartbeat_status.go) so the web roster's dots agree with
// `ppz who`. Kept as a local copy rather than importing internal/daemon —
// the HTTP server has no other reason to depend on the daemon package, and
// the rule is a stable 15-line domain fact. A parity test pins the
// thresholds (TestClassifyAgentStatus).
func classifyAgentStatus(last, now time.Time, intervalSec int) string {
	if last.IsZero() {
		return "offline"
	}
	if intervalSec <= 0 {
		intervalSec = 60
	}
	interval := time.Duration(intervalSec) * time.Second
	age := now.Sub(last)
	if age < 0 {
		return "online"
	}
	if 2*age < 3*interval {
		return "online"
	}
	if age < 3*interval {
		return "stale"
	}
	return "offline"
}

// buildChatRoster splits sources into AGENTS (pty) and INBOXES (message),
// lists pipes as PIPES, and stamps agent liveness. Each section is sorted so
// the menu is stable across reloads: agents/inboxes by handle, pipes by
// (manifold, name) — matching the org page's pipe ordering convention.
func buildChatRoster(sources []chatSourceInput, pipes []chatPipeInput, now time.Time) chatRoster {
	var r chatRoster
	for _, s := range sources {
		switch s.Source.Kind {
		case db.SourceKindPTY:
			status := classifyAgentStatus(s.HeartbeatAt, now, s.IntervalSec)
			r.Agents = append(r.Agents, chatEntry{
				Kind:      chatKindAgent,
				Target:    s.Source.Handle,
				Label:     s.Source.Handle,
				Namespace: s.Source.Manifold,
				Status:    status,
				State:     s.AgentState,
				HasStatus: true,
				Title:     chatTitle(chatKindAgent, s.Source.Handle, status, s.AgentState),
			})
		default: // message
			r.Inboxes = append(r.Inboxes, chatEntry{
				Kind:      chatKindInbox,
				Target:    s.Source.Handle,
				Label:     s.Source.Handle,
				Namespace: s.Source.Manifold,
				Title:     chatTitle(chatKindInbox, s.Source.Handle, "", ""),
			})
		}
	}
	for _, p := range pipes {
		target := p.Name
		if p.Manifold != "" {
			target = p.Manifold + "." + p.Name
		}
		r.Pipes = append(r.Pipes, chatEntry{
			Kind:      chatKindPipe,
			Target:    target,
			Label:     p.Name,
			Namespace: p.Manifold,
			Title:     chatTitle(chatKindPipe, p.Name, "", ""),
		})
	}
	sort.Slice(r.Agents, func(i, j int) bool { return r.Agents[i].Target < r.Agents[j].Target })
	sort.Slice(r.Inboxes, func(i, j int) bool { return r.Inboxes[i].Target < r.Inboxes[j].Target })
	sort.Slice(r.Pipes, func(i, j int) bool {
		if r.Pipes[i].Namespace != r.Pipes[j].Namespace {
			return r.Pipes[i].Namespace < r.Pipes[j].Namespace
		}
		return r.Pipes[i].Label < r.Pipes[j].Label
	})
	return r
}

// Empty returns true when the roster has no rows at all.
func (r chatRoster) Empty() bool {
	return len(r.Agents)+len(r.Inboxes)+len(r.Pipes) == 0
}

// chatWindow is the resolved coordinates of a chat window: the JetStream
// subject to publish to and the stream name to read/follow.
//
// For pipe windows the manifold travels in the target, so Subject/StreamName
// are fully resolved here. For source windows the manifold is a property of
// the DB source row (not the URL), so this pure resolver only validates the
// handle and leaves Subject/StreamName empty — resolveChatWindowDB fills them
// from the source's real manifold. (The web console addresses a source by
// bare handle at the root manifold, matching the CLI's `ppz source create
// HANDLE` and the TUI's handle-keyed DMs; a same-handle source under a
// non-root manifold — only reachable via the raw API — isn't addressable
// distinctly here yet.)
type chatWindow struct {
	Kind       string // "source" | "pipe" (backend kinds; agents+inboxes are both "source")
	Manifold   string
	Handle     string // set for source windows (empty for pipes)
	Name       string // pipe leaf name (empty for source windows — they use "inbox")
	Subject    string // pipe windows only; source windows resolved from the DB row
	StreamName string // pipe windows only; source windows resolved from the DB row
}

// resolveChatWindow validates a (kind, target) pair from the URL so a guessed
// URL can't reach an arbitrary subject. Two backend kinds:
//
//	source: target = <handle>            -> <handle>.inbox (subject built by DB layer)
//	pipe:   target = <manifold>.<name>   -> uncollared pipe stream (fully built here)
func resolveChatWindow(acct uuid.UUID, kind, target string) (chatWindow, error) {
	switch kind {
	case "source":
		if err := natsubj.ValidateHandle(target); err != nil {
			return chatWindow{}, errors.New("invalid source handle")
		}
		return chatWindow{Kind: "source", Handle: target, Name: "inbox"}, nil
	case "pipe":
		manifold, name := splitManifoldName(target)
		if err := natsubj.ValidatePipe(name); err != nil {
			return chatWindow{}, errors.New("invalid pipe name")
		}
		if manifold != "" {
			for _, seg := range strings.Split(manifold, ".") {
				if err := natsubj.ValidateHandle(seg); err != nil {
					return chatWindow{}, errors.New("invalid pipe manifold")
				}
			}
		}
		return chatWindow{
			Kind:       "pipe",
			Manifold:   manifold,
			Name:       name,
			Subject:    natsubj.BuildSubject(acct, manifold, "", name),
			StreamName: natsubj.BuildStreamName(acct, manifold, "", name),
		}, nil
	default:
		return chatWindow{}, errors.New("unknown chat window kind")
	}
}
