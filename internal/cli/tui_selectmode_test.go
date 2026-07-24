package cli

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// Mouse capture starts on (the program launches WithMouseCellMotion), so the
// select-mode escape hatch has something to toggle off.
func TestMouse_StartsOn(t *testing.T) {
	m := newTUIModel("me", "s", "/tmp/x.sock", make(chan tea.Msg, 8), context.Background())
	if !m.mouseOn {
		t.Fatal("mouse capture should start on")
	}
}

// `m` in menu focus toggles mouse capture off (so the terminal can drag-select
// for copy) and back on, each returning a command for bubbletea to apply.
func TestMouse_ToggleKey(t *testing.T) {
	m := newTUIModel("me", "s", "/tmp/x.sock", make(chan tea.Msg, 8), context.Background())
	m.agents = append(m.agents, tItem{kind: kAgent, key: "alice", label: "alice"})

	var mm tea.Model = m
	mm, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	if mm.(tuiModel).mouseOn {
		t.Fatal("first `m` should turn mouse capture off")
	}
	if cmd == nil || cmd() == nil {
		t.Fatal("toggling off should emit a DisableMouse command")
	}

	mm, cmd = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	if !mm.(tuiModel).mouseOn {
		t.Fatal("second `m` should turn mouse capture back on")
	}
	if cmd == nil || cmd() == nil {
		t.Fatal("toggling on should emit an EnableMouseCellMotion command")
	}
}

// In the chat input `m` is a literal character, not the toggle — typing must
// not silently disable the mouse mid-message.
func TestMouse_NotToggledWhileTyping(t *testing.T) {
	m := newTUIModel("me", "s", "/tmp/x.sock", make(chan tea.Msg, 8), context.Background())
	m.agents = append(m.agents, tItem{kind: kAgent, key: "alice", label: "alice"})
	m.focus = fChat
	m.chatTi.Focus()

	var mm tea.Model = m
	mm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	if !mm.(tuiModel).mouseOn {
		t.Error("`m` while typing must not toggle mouse capture")
	}
	if got := mm.(tuiModel).chatTi.Value(); got != "m" {
		t.Errorf("`m` while typing should reach the input, got %q", got)
	}
}

// The help bar surfaces the state: a SELECT MODE banner when capture is off, and
// the `m copy-mode` hint when it's on.
func TestMouse_HelpBarReflectsState(t *testing.T) {
	m := newInboxModel(t) // sized, menu focus, mouse on

	on := ansi.Strip(m.helpBar())
	if !strings.Contains(on, "copy-mode") {
		t.Errorf("menu help should hint the copy-mode toggle, got %q", on)
	}

	m.mouseOn = false
	off := ansi.Strip(m.helpBar())
	if !strings.Contains(off, "SELECT MODE") {
		t.Errorf("help bar should announce SELECT MODE when capture is off, got %q", off)
	}
}
