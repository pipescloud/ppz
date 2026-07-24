package cli

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pipescloud/ppz/internal/cliproto"
)

func pipeKeys(m tuiModel) []string {
	out := make([]string, 0, len(m.pipes))
	for _, p := range m.pipes {
		out = append(out, p.key)
	}
	return out
}

func hasPipe(m tuiModel, name string) bool {
	for _, p := range m.pipes {
		if p.key == name {
			return true
		}
	}
	return false
}

// rootUncollaredNames keeps root-namespace pipes (addressable as bare targets)
// and drops manifold-qualified ones the bare-target machinery can't reach.
func TestRootUncollaredNames_FiltersManifold(t *testing.T) {
	got := rootUncollaredNames([]cliproto.UncollaredPipe{
		{Name: "standup"},
		{Manifold: "team-a", Name: "chat"}, // not addressable as a bare target
		{Name: "lobby"},
	})
	if len(got) != 2 || got[0] != "standup" || got[1] != "lobby" {
		t.Fatalf("rootUncollaredNames = %v, want [standup lobby]", got)
	}
}

// autoAddPipes adds a row + follow for each discovered uncollared pipe, and is
// idempotent — a second poll of the same set adds nothing and starts no second
// follow.
func TestAutoAddPipes_AddsAndIsIdempotent(t *testing.T) {
	m := newTUIModel("me", "s", "/tmp/x.sock", make(chan tea.Msg, 8), context.Background())
	m.autoAddPipes([]string{"standup", "lobby"})
	if got := pipeKeys(m); len(got) != 2 || got[0] != "standup" || got[1] != "lobby" {
		t.Fatalf("after first discovery pipes = %v, want [standup lobby]", got)
	}
	if !m.followed["standup"] || !m.followed["lobby"] {
		t.Fatalf("discovered pipes should be followed: %v", m.followed)
	}

	// A repeat poll (level-triggered) must not duplicate rows.
	m.autoAddPipes([]string{"standup", "lobby"})
	if got := pipeKeys(m); len(got) != 2 {
		t.Fatalf("repeat discovery duplicated rows: %v", got)
	}
}

// Auto-discovery must not move the user's selection. A new pipe appended while
// the user has an agent (or earlier row) selected leaves the cursor put.
func TestAutoAddPipes_PreservesSelection(t *testing.T) {
	m := newTUIModel("me", "s", "/tmp/x.sock", make(chan tea.Msg, 8), context.Background())
	m.agents = append(m.agents, tItem{kind: kAgent, key: "alice", label: "alice"})
	m.sel = 0 // on alice

	m.autoAddPipes([]string{"standup"})
	if m.sel != 0 {
		t.Fatalf("auto-add moved selection off alice: sel=%d", m.sel)
	}
	if !hasPipe(m, "standup") {
		t.Fatalf("standup not auto-added")
	}

	// A manual add, by contrast, does select the new pipe.
	m.addPipe("manual")
	if m.flatItem(m.sel).key != "manual" {
		t.Fatalf("manual add should select the new pipe, sel key = %q", m.flatItem(m.sel).key)
	}
}

// A pipe the user removed with `-` this session must not be re-added by a
// subsequent discovery poll (which still lists it as uncollared).
func TestAutoAddPipes_RespectsDismissal(t *testing.T) {
	m := newTUIModel("me", "s", "/tmp/x.sock", make(chan tea.Msg, 8), context.Background())
	m.pipeCancels["standup"] = func() {}
	m.autoAddPipes([]string{"standup"})

	flat := len(m.agents) + len(m.sources) // first pipe row
	m.sel = flat
	m.removePipe(flat)
	if hasPipe(m, "standup") {
		t.Fatalf("standup not removed")
	}

	// Next poll still reports it — it must stay gone.
	m.autoAddPipes([]string{"standup"})
	if hasPipe(m, "standup") {
		t.Fatalf("dismissed pipe was re-added by discovery")
	}
}

// The discovery message flows through Update the same way live stream events do,
// and keeps pumping the event channel.
func TestUpdate_PipesDiscoveredMsg(t *testing.T) {
	m := newInboxModel(t)
	var mm tea.Model = m
	mm, cmd := mm.Update(pipesDiscoveredMsg{names: []string{"standup"}})
	if !hasPipe(mm.(tuiModel), "standup") {
		t.Fatalf("pipesDiscoveredMsg did not add the pipe")
	}
	if cmd == nil {
		t.Fatalf("pipesDiscoveredMsg should keep pumping the event channel")
	}
}
