package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// captureComplete runs cmdComplete with the given args and returns
// stdout as the list of completion candidates the shell would receive.
// We swap os.Stdout for a pipe — cmdComplete writes directly to stdout
// (matching the shell-hook contract) so capturing it is the only way.
func captureComplete(t *testing.T, args []string) []string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	if err := cmdComplete(args); err != nil {
		w.Close()
		t.Fatalf("cmdComplete: %v", err)
	}
	w.Close()

	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	out := b.String()
	if out == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(out, "\n"), "\n")
}

// TestComplete_TopLevel: `ppz <tab>` lists every top-level verb.
func TestComplete_TopLevel(t *testing.T) {
	got := captureComplete(t, []string{""})
	if !contains(got, "status") || !contains(got, "pipe") || !contains(got, "read") {
		t.Errorf("expected top-level verbs, got %v", got)
	}
}

// TestComplete_PrefixFilter: `ppz s<tab>` filters to s-prefixed verbs.
func TestComplete_PrefixFilter(t *testing.T) {
	got := captureComplete(t, []string{"s"})
	for _, g := range got {
		if !strings.HasPrefix(g, "s") {
			t.Errorf("got non-s prefix: %q", g)
		}
	}
	// At minimum: send, status.
	for _, want := range []string{"send", "status"} {
		if !contains(got, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
}

// TestComplete_Subverb: `ppz pipe <tab>` returns every pipe subverb.
// `acl` joined create/set/destroy in ACL Phase 2.
func TestComplete_Subverb(t *testing.T) {
	got := captureComplete(t, []string{"pipe", ""})
	want := []string{"create", "set", "destroy", "acl"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, w := range want {
		if !contains(got, w) {
			t.Errorf("missing %q in %v", w, got)
		}
	}
}

// TestComplete_SubverbPrefix: `ppz pipe cr<tab>` narrows to "create".
func TestComplete_SubverbPrefix(t *testing.T) {
	got := captureComplete(t, []string{"pipe", "cr"})
	if len(got) != 1 || got[0] != "create" {
		t.Errorf("got %v, want [create]", got)
	}
}

// TestComplete_DashDashStripped: the shell script passes `--` as the
// first arg; cmdComplete must skip it. Without this, a verb at index 0
// would be misinterpreted as a partial.
func TestComplete_DashDashStripped(t *testing.T) {
	got := captureComplete(t, []string{"--", "pipe", ""})
	if !contains(got, "create") {
		t.Errorf("expected pipe subverbs after --, got %v", got)
	}
}

// TestComplete_UnknownVerb: `ppz nope X<tab>` returns nothing — we
// don't speculate about a verb we don't know.
func TestComplete_UnknownVerb(t *testing.T) {
	got := captureComplete(t, []string{"nope", "X"})
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

// TestComplete_TopLevel_IncludesCommand: `ppz <tab>` must include "command".
func TestComplete_TopLevel_IncludesCommand(t *testing.T) {
	got := captureComplete(t, []string{""})
	if !contains(got, "command") {
		t.Errorf("expected 'command' in top-level completions, got %v", got)
	}
}

// TestComplete_TopLevel_MatchesDispatchTable: every verb root.go's
// Run() switch dispatches must appear at `ppz <tab>`. This parses
// root.go at test time rather than hardcoding a list — adding a new
// verb without updating completion's topLevelVerbs fails CI
// automatically, regardless of what the new verb is named.
//
// Excluded verbs are internal/help shortcuts that aren't meant for
// everyday tab completion: __complete (hidden — invoked by the shell
// hook), completion (one-shot setup verb), and the help triplet
// (-h / --help / help — universal shell idioms, not surfaces we tab
// users to discover).
func TestComplete_TopLevel_MatchesDispatchTable(t *testing.T) {
	src, err := os.ReadFile("root.go")
	if err != nil {
		t.Fatalf("read root.go: %v", err)
	}

	// `case "verb":` (or `case "v1", "v2":`) appears only inside Run's
	// top-level switch — root.go has no other switches today. The
	// outer regex captures the comma-separated quoted-string list; the
	// inner regex pulls each verb out.
	caseRe := regexp.MustCompile(`case\s+("[^"]+"(?:\s*,\s*"[^"]+")*)\s*:`)
	stringRe := regexp.MustCompile(`"([^"]+)"`)

	excluded := map[string]bool{
		"__complete": true, // hidden — invoked by the shell hook itself
		"completion": true, // one-shot setup verb
		"tui":        true, // hidden alias for `chat`
		"-h":         true, // shell-idiom flags, not ppz verbs
		"--help":     true,
		"help":       true,
	}

	got := captureComplete(t, []string{""})
	have := map[string]bool{}
	for _, v := range got {
		have[v] = true
	}

	dispatched := map[string]bool{}
	for _, m := range caseRe.FindAllStringSubmatch(string(src), -1) {
		for _, s := range stringRe.FindAllStringSubmatch(m[1], -1) {
			verb := s[1]
			if excluded[verb] {
				continue
			}
			dispatched[verb] = true
		}
	}
	if len(dispatched) == 0 {
		t.Fatal("root.go parse found zero dispatched verbs — the regex is stale")
	}

	for verb := range dispatched {
		if !have[verb] {
			t.Errorf("verb %q is dispatched by root.go but missing from topLevelVerbs", verb)
		}
	}
}

// TestComplete_AgentSubverb: `ppz agent <tab>` → [create].
// agent is grouped (see cmdAgentGroup) — without a subverb entry tab
// falls into the "unknown subgroup" silent path.
func TestComplete_AgentSubverb(t *testing.T) {
	got := captureComplete(t, []string{"agent", ""})
	if !contains(got, "create") {
		t.Errorf("expected 'create' under agent, got %v", got)
	}
}

// TestComplete_DaemonSubverbIncludesRestart: `ppz daemon <tab>` must
// include 'restart'. The verb is wired (daemon.go:30, documented at
// usage line 251) but absent from the subverbs table.
func TestComplete_DaemonSubverbIncludesRestart(t *testing.T) {
	got := captureComplete(t, []string{"daemon", ""})
	if !contains(got, "restart") {
		t.Errorf("expected 'restart' under daemon, got %v", got)
	}
}

// TestComplete_SubsSubverb: `ppz subs <tab>` → all five subverbs.
func TestComplete_SubsSubverb(t *testing.T) {
	got := captureComplete(t, []string{"subs", ""})
	for _, want := range []string{"ls", "add", "rm", "wait", "read"} {
		if !contains(got, want) {
			t.Errorf("expected %q under subs, got %v", want, got)
		}
	}
}

// injectFakeSources swaps the daemon-listing seam for a canned
// snapshot so target/handle completion paths can be exercised
// hermetically. t.Cleanup restores the live implementation.
//
// Mutates a package-level var — callers must NOT mark their test
// t.Parallel(). If parallelism is ever wanted here, switch the seam
// to an interface field on a per-call context the dispatcher reads
// (e.g. cmdComplete signature change).
func injectFakeSources(t *testing.T, sources []cliproto.Source) {
	t.Helper()
	orig := listSourcesForCompletion
	listSourcesForCompletion = func() []cliproto.Source { return sources }
	t.Cleanup(func() { listSourcesForCompletion = orig })
}

// fakeSources is the canned source set used by every target-slot test.
// Two handles, three pipes total — enough to exercise prefix filtering
// and handle-vs-target distinction.
func fakeSources() []cliproto.Source {
	return []cliproto.Source{
		{Handle: "alice", PipeInfos: []cliproto.PipeInfo{{Pipe: "inbox"}, {Pipe: "stdout"}}},
		{Handle: "bob", PipeInfos: []cliproto.PipeInfo{{Pipe: "inbox"}}},
	}
}

// TestComplete_PipeDestroy_Targets: `ppz pipe destroy <tab>` completes
// existing <handle>.<pipe> so users can pick a real target. The verb
// also accepts uncollared names — covered separately once the uncollared
// path is wired.
func TestComplete_PipeDestroy_Targets(t *testing.T) {
	injectFakeSources(t, fakeSources())
	got := captureComplete(t, []string{"pipe", "destroy", ""})
	for _, want := range []string{"alice.inbox", "alice.stdout", "bob.inbox"} {
		if !contains(got, want) {
			t.Errorf("expected %q in pipe destroy completions, got %v", want, got)
		}
	}
}

// TestComplete_SourceDestroy_HandlesAndTargets: `ppz source destroy <tab>`
// completes both bare handles AND handle.pipe — usage doc states both
// are valid pattern targets.
func TestComplete_SourceDestroy_HandlesAndTargets(t *testing.T) {
	injectFakeSources(t, fakeSources())
	got := captureComplete(t, []string{"source", "destroy", ""})
	for _, want := range []string{"alice", "bob", "alice.inbox", "bob.inbox"} {
		if !contains(got, want) {
			t.Errorf("expected %q in source destroy completions, got %v", want, got)
		}
	}
}

// TestComplete_SubsAdd_Targets: `ppz subs add <tab>` completes targets,
// same vocabulary as send/read so users learn one rule.
func TestComplete_SubsAdd_Targets(t *testing.T) {
	injectFakeSources(t, fakeSources())
	got := captureComplete(t, []string{"subs", "add", ""})
	for _, want := range []string{"alice.inbox", "bob.inbox"} {
		if !contains(got, want) {
			t.Errorf("expected %q in subs add completions, got %v", want, got)
		}
	}
}

// TestComplete_SubsAdd_RepeatedTargets: `ppz subs add a.b <tab>` —
// subs add takes a variadic target list, so the 2nd+ positionals must
// also complete targets, not silently drop into "no completion".
func TestComplete_SubsAdd_RepeatedTargets(t *testing.T) {
	injectFakeSources(t, fakeSources())
	got := captureComplete(t, []string{"subs", "add", "alice.inbox", ""})
	if !contains(got, "bob.inbox") {
		t.Errorf("expected target completion on repeated subs add positional, got %v", got)
	}
}

// TestComplete_SubsRm_Targets: `ppz subs rm <tab>` mirrors subs add —
// answer #1 said "same as send/read targets", not "subscribed only".
func TestComplete_SubsRm_Targets(t *testing.T) {
	injectFakeSources(t, fakeSources())
	got := captureComplete(t, []string{"subs", "rm", ""})
	if !contains(got, "alice.inbox") || !contains(got, "bob.inbox") {
		t.Errorf("expected targets under subs rm, got %v", got)
	}
}

// TestComplete_SubsRm_RepeatedTargets: variadic, same as add.
func TestComplete_SubsRm_RepeatedTargets(t *testing.T) {
	injectFakeSources(t, fakeSources())
	got := captureComplete(t, []string{"subs", "rm", "alice.inbox", ""})
	if !contains(got, "bob.inbox") {
		t.Errorf("expected target completion on repeated subs rm positional, got %v", got)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
