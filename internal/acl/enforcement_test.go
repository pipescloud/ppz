package acl

import (
	"encoding/json"
	"strings"
	"testing"
)

// ACL Phase 3 — every surface reports whether it is enforced.
//
// A screen that says "bob cannot read alice.stdout" while bob
// demonstrably can is worse than no screen: it is a security control
// that lies, and it lies most convincingly to the person checking
// whether they are safe. Because enforcement is opt-in per org, that
// state is not inferable from the answer itself and has to travel with
// it.

func unenforcedWhoami() WhoamiView {
	owner, bob := alice(), stranger()
	return WhoamiView{
		Pipe:      "alice.stdout",
		Principal: "bob",
		Decision:  Evaluate(bob, collared(owner, "stdout"), nil),
		Enforced:  false,
	}
}

func TestWhoami_ReportsNotEnforced(t *testing.T) {
	var b strings.Builder
	RenderWhoami(&b, unenforcedWhoami())
	out := strings.ToLower(b.String())

	if !strings.Contains(out, "not enforced") {
		t.Errorf("an unenforced answer must say so — otherwise it reads as a guarantee:\n%s", b.String())
	}
}

// The enforced case must NOT carry the caveat, or the warning becomes
// wallpaper and stops being read.
func TestWhoami_OmitsCaveatWhenEnforced(t *testing.T) {
	v := unenforcedWhoami()
	v.Enforced = true

	// Positive control: the identical view with Enforced=false MUST
	// carry the caveat. Without this pairing, a renderer that never
	// emits it passes trivially.
	var off strings.Builder
	RenderWhoami(&off, unenforcedWhoami())
	if !strings.Contains(strings.ToLower(off.String()), "not enforced") {
		t.Fatalf("control: the unenforced view must carry the caveat:\n%s", off.String())
	}

	var b strings.Builder
	RenderWhoami(&b, v)

	if strings.Contains(strings.ToLower(b.String()), "not enforced") {
		t.Errorf("an enforced answer must not be labelled unenforced:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "alice.stdout") {
		t.Fatalf("control: the view rendered nothing:\n%s", b.String())
	}
}

// Agents branch on this, so it has to be a field rather than prose.
func TestWhoami_JSONCarriesEnforcedFlag(t *testing.T) {
	raw, err := json.Marshal(unenforcedWhoami())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"enforced":false`) {
		t.Errorf("whoami JSON must carry enforced:\n%s", raw)
	}

	v := unenforcedWhoami()
	v.Enforced = true
	raw, _ = json.Marshal(v)
	if !strings.Contains(string(raw), `"enforced":true`) {
		t.Errorf("enforced:true must round-trip:\n%s", raw)
	}
}

// The roster is the other surface an admin reads to decide whether they
// are safe, so it carries the same flag.
func TestRoster_ReportsEnforcementState(t *testing.T) {
	owner := alice()
	rows := []RosterRow{{Principal: "foo", Decision: Evaluate(owner, stdoutOf(owner), nil)}}

	var b strings.Builder
	RenderPipeRoster(&b, "alice.stdout", Roster{Rows: rows, Enforced: false})
	if !strings.Contains(strings.ToLower(b.String()), "not enforced") {
		t.Errorf("an unenforced roster must say so:\n%s", b.String())
	}

	b.Reset()
	RenderPipeRoster(&b, "alice.stdout", Roster{Rows: rows, Enforced: true})
	if strings.Contains(strings.ToLower(b.String()), "not enforced") {
		t.Errorf("an enforced roster must not be labelled unenforced:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "foo") {
		t.Fatalf("control: the roster rendered no rows:\n%s", b.String())
	}
}
