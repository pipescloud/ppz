package cli

import (
	"os"
	"strings"
	"testing"
)

// `agent prompt` echoes the exact prompt `agent run` / `agent create`
// would boot the harness with. With no positional prompt it renders the
// per-harness default for the handle — byte-for-byte what the other two
// verbs resolve — so a user can preview (or pipe) the boot prompt without
// launching anything.
func TestAgentPromptFor_EchoesClaudeDefault(t *testing.T) {
	got, err := agentPromptFor([]string{"ash"})
	if err != nil {
		t.Fatalf("agentPromptFor: %v", err)
	}
	want := defaultAgentPrompt("ash", "claude")
	if got != want {
		t.Errorf("agent prompt should echo the claude default for the handle;\n got=%q\nwant=%q", got, want)
	}
}

// A positional prompt is echoed verbatim — the same string the harness
// would receive as its initial prompt under run/create.
func TestAgentPromptFor_EchoesPositionalPrompt(t *testing.T) {
	got, err := agentPromptFor([]string{"ash", "hello world"})
	if err != nil {
		t.Fatalf("agentPromptFor: %v", err)
	}
	if got != "hello world" {
		t.Errorf("prompt=%q, want %q", got, "hello world")
	}
}

// The harness flag selects which default prompt is rendered, matching
// create/run — codex's default differs from claude's.
func TestAgentPromptFor_HarnessSelectsDefault(t *testing.T) {
	got, err := agentPromptFor([]string{"--codex", "ash"})
	if err != nil {
		t.Fatalf("agentPromptFor: %v", err)
	}
	want := defaultAgentPrompt("ash", "codex")
	if got != want {
		t.Errorf("agent prompt --codex should echo codex's default;\n got=%q\nwant=%q", got, want)
	}
	// Guard: codex and claude defaults genuinely differ, so the check
	// above isn't vacuously true.
	if want == defaultAgentPrompt("ash", "claude") {
		t.Fatal("codex and claude defaults are identical; the harness-selection test can't distinguish them")
	}
}

// --prompt-file content is echoed — the same source run/create read the
// initial prompt from.
func TestAgentPromptFor_EchoesPromptFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/p.txt"
	if err := os.WriteFile(path, []byte("from file"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := agentPromptFor([]string{"--prompt-file", path, "ash"})
	if err != nil {
		t.Fatalf("agentPromptFor: %v", err)
	}
	if got != "from file" {
		t.Errorf("prompt=%q, want %q", got, "from file")
	}
}

// Missing handle: the default prompt is handle-specific (it substitutes
// the handle into the cheat sheet), so a handle is required — and the
// usage error names the `prompt` verb the user typed.
func TestAgentPromptFor_RequiresHandle(t *testing.T) {
	_, err := agentPromptFor(nil)
	if err == nil {
		t.Fatal("expected error for missing handle")
	}
	if !strings.Contains(err.Error(), "prompt") {
		t.Errorf("missing-handle error should name the `prompt` verb, got: %q", err.Error())
	}
}
