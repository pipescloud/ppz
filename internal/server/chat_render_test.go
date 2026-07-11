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
			{Kind: chatKindAgent, Target: "claude", Label: "claude", Status: "online", State: "working", HasStatus: true, Title: "claude · dm · online|working"},
			{Kind: chatKindAgent, Target: "codex", Label: "codex", Status: "offline", HasStatus: true, Title: "codex · dm · offline"},
		},
		Inboxes: []chatEntry{
			{Kind: chatKindInbox, Target: "ops", Label: "ops", Title: "ops · dm · inbox", Unread: 3},
		},
		Pipes: []chatEntry{
			{Kind: chatKindPipe, Target: "eng.backend", Label: "backend", Namespace: "eng", Title: "#backend · pipe (uncollared)"},
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
		// TUI-parity chat-pane titles carried per row for the JS to display.
		`data-chat-title="claude · dm · online|working"`,
		`data-chat-title="ops · dm · inbox"`,
		`data-chat-title="#backend · pipe (uncollared)"`,
		// Top-bar summary counts (1 of the 2 agents is online).
		`1 online · 2 agents · 1 pipes`,
		// Unread badge on the ops inbox (3 unread); the badge carries the count.
		`chat-unread`,
		`>3<`,
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

// When the viewer owns message handles, the composer renders a handle picker
// (the "send as" identity, analog of the CLI current handle) listing exactly
// those handles.
func TestChatTemplate_HandlePicker_WhenOwned(t *testing.T) {
	data := map[string]any{
		"Org":     db.Account{ID: uuid.New(), Name: "alpha"},
		"Roster":  chatRoster{},
		"Me":      "foo",
		"Handles": []string{"desk", "ops"},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "chat.html", data); err != nil {
		t.Fatalf("render chat.html: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`chat-picker`,
		`data-handle="desk"`,
		`data-handle="ops"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("handle picker output missing %q", want)
		}
	}
}

// When the viewer owns no handles, there is no picker; instead a notice points
// them at `ppz source create` and the composer stays unusable (block-send).
func TestChatTemplate_HandlePicker_WhenNone(t *testing.T) {
	data := map[string]any{
		"Org":     db.Account{ID: uuid.New(), Name: "alpha"},
		"Roster":  chatRoster{},
		"Me":      "foo",
		"Handles": []string{},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "chat.html", data); err != nil {
		t.Fatalf("render chat.html: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `chat-no-handle`) {
		t.Error("expected a no-handle notice when the viewer owns no handles")
	}
	if !strings.Contains(out, "ppz source create") {
		t.Error("no-handle notice should point the user at `ppz source create`")
	}
	if strings.Contains(out, `chat-picker-btn`) {
		t.Error("no handle picker should render when the viewer owns none")
	}
}
