package cli

import (
	"strings"
	"testing"
)

// A verb users can't discover doesn't exist. `pipe set` has to show up in
// three places that are each independently authoritative for discovery:
// the grouped top-level help, its own `ppz help pipe set` body, and shell
// tab-completion.

func TestHelp_PipeSetHasItsOwnTopic(t *testing.T) {
	body, ok := helpTopics["pipe set"]
	if !ok || strings.TrimSpace(body) == "" {
		t.Fatal(`helpTopics["pipe set"] missing — 'ppz pipe set --help' would fall back to the group page`)
	}
	for _, flag := range []string{"--ttl", "--max-msgs", "--max-bytes"} {
		if !strings.Contains(body, flag) {
			t.Errorf("pipe set help does not document %s", flag)
		}
	}
}

// The `pipe` group page enumerates its subverbs; `set` joining
// create/destroy has to be reflected there too.
func TestHelp_PipeGroupListsSet(t *testing.T) {
	body := helpTopics["pipe"]
	if !strings.Contains(body, "set") {
		t.Errorf("`ppz help pipe` does not mention the set subverb:\n%s", body)
	}
}

func TestHelp_TopLevelPipesGroupListsSet(t *testing.T) {
	var rows []string
	for _, g := range topLevelGroups {
		if g.title != "PIPES" {
			continue
		}
		for _, r := range g.rows {
			rows = append(rows, r.sig)
		}
	}
	if len(rows) == 0 {
		t.Fatal("no PIPES group in the top-level help")
	}
	for _, sig := range rows {
		if strings.HasPrefix(sig, "ppz pipe set") {
			return
		}
	}
	t.Errorf("top-level PIPES group has no `ppz pipe set` row, got %v", rows)
}

func TestComplete_PipeSubverbsIncludeSet(t *testing.T) {
	got := captureComplete(t, []string{"pipe", ""})
	if !contains(got, "set") {
		t.Errorf("`ppz pipe <tab>` does not offer set, got %v", got)
	}
}

// `pipe set` targets an EXISTING pipe, so — unlike create — its first
// positional completes from the daemon's known <handle>.<pipe> set.
func TestComplete_PipeSet_CompletesExistingTargets(t *testing.T) {
	if !pipeSubverbsTakingTargets["set"] {
		t.Error("pipeSubverbsTakingTargets missing \"set\" — `ppz pipe set <tab>` would offer nothing")
	}
}
