package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pipescloud/ppz/internal/db"
)

// The chat console template renders all three roster sections with stable
// data- attributes so both the browser JS and the e2e suite can locate rows
// without depending on layout. Rendered directly (no DB) against a
// hand-built roster.
func TestChatTemplate_RendersThreeSections(t *testing.T) {
	roster := chatRoster{
		Agents: []chatEntry{
			{Kind: chatKindAgent, Target: "claude", Label: "claude", Status: "online", State: "working", HasStatus: true},
			{Kind: chatKindAgent, Target: "codex", Label: "codex", Status: "offline", HasStatus: true},
		},
		Inboxes: []chatEntry{
			{Kind: chatKindInbox, Target: "ops", Label: "ops"},
		},
		Pipes: []chatEntry{
			{Kind: chatKindPipe, Target: "eng.backend", Label: "backend", Namespace: "eng"},
		},
	}
	data := map[string]any{
		"Org":    db.Account{ID: uuid.New(), Name: "alpha"},
		"Roster": roster,
		"Me":     "james",
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "chat.html", data); err != nil {
		t.Fatalf("render chat.html: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`data-section="agents"`,
		`data-section="inboxes"`,
		`data-section="pipes"`,
		// AGENTS section rows, keyed kind:target, carrying liveness.
		`data-chat-entry="agent:claude"`,
		`data-chat-status="online"`,
		`data-chat-state="working"`,
		`data-chat-entry="agent:codex"`,
		`data-chat-status="offline"`,
		// INBOXES + PIPES rows.
		`data-chat-entry="inbox:ops"`,
		`data-chat-entry="pipe:eng.backend"`,
		// Composer + live viewport mount points the JS wires to.
		`id="chat-log"`,
		`id="chat-input"`,
		// The viewer's identity (used as the outbound sender) is exposed.
		`data-me="james"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("chat.html output missing %q", want)
		}
	}
}

// The console page must reference the chat.js asset that drives it.
func TestChatTemplate_LoadsChatJS(t *testing.T) {
	data := map[string]any{
		"Org":    db.Account{ID: uuid.New(), Name: "alpha"},
		"Roster": chatRoster{},
		"Me":     "james",
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "chat.html", data); err != nil {
		t.Fatalf("render chat.html: %v", err)
	}
	if !strings.Contains(buf.String(), "/assets/chat.js") {
		t.Error("chat.html does not load /assets/chat.js")
	}
}
