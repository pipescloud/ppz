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
