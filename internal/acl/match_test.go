package acl

import "testing"

// ACL Phase 2 — selector matching.
//
// A selector matches a pipe's SUBJECT PATH: what natsubj.BuildSubject
// produces, minus the account prefix. `*` is one token, `**` is one or
// more trailing tokens — the same shapes NATS uses for `*` and `>`,
// which is what makes the Phase 3 credential compiler close to
// mechanical.
//
// The manifold/collar ambiguity is inherited deliberately: natsubj
// already documents that `acct.X.pipe` is indistinguishable at the wire
// level between a manifold and a source segment. Selectors match the
// path, so they inherit exactly that and introduce nothing new.

func TestMatch_Literal(t *testing.T) {
	cases := []struct {
		sel  Selector
		path string
		want bool
	}{
		{"alice.inbox", "alice.inbox", true},
		{"alice.inbox", "alice.stdout", false},
		{"alice.inbox", "bob.inbox", false},
		{"alice.inbox", "alice", false},
		{"alice.inbox", "alice.inbox.extra", false},
		{"room", "room", true},
		{"room", "other", false},
	}
	for _, c := range cases {
		if got := Match(c.sel, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.sel, c.path, got, c.want)
		}
	}
}

func TestMatch_SingleToken(t *testing.T) {
	cases := []struct {
		sel  Selector
		path string
		want bool
	}{
		{"alice.*", "alice.inbox", true},
		{"alice.*", "alice.stdout", true},
		{"alice.*", "bob.inbox", false},
		{"*.inbox", "alice.inbox", true},
		{"*.inbox", "bob.inbox", true},
		{"*.inbox", "alice.stdout", false},
		{"*", "room", true},
		{"*", "alice.inbox", false},
	}
	for _, c := range cases {
		if got := Match(c.sel, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.sel, c.path, got, c.want)
		}
	}
}

// `*` must never span a dot. A selector meant to grant "alice's pipes"
// that quietly matched "alice.team.secret" would be a silent
// over-grant, and the compiler would faithfully turn it into NATS
// permissions.
func TestMatch_SingleTokenDoesNotSpanTokens(t *testing.T) {
	// Positive control: a matcher that always says no would satisfy
	// every assertion below without matching anything.
	if !Match("alice.*", "alice.inbox") {
		t.Fatal(`control: Match("alice.*", "alice.inbox") must be true`)
	}
	if Match("alice.*", "alice.team.secret") {
		t.Error(`Match("alice.*", "alice.team.secret") = true — * must match exactly one token`)
	}
	if Match("*", "a.b") {
		t.Error(`Match("*", "a.b") = true — * must match exactly one token`)
	}
}

func TestMatch_MultiToken(t *testing.T) {
	cases := []struct {
		sel  Selector
		path string
		want bool
	}{
		{"alice.**", "alice.inbox", true},
		{"alice.**", "alice.team.secret", true},
		{"alice.**", "bob.inbox", false},
		// `**` is one-or-more, matching NATS `>`. A grant on
		// "alice.**" is about what's under alice, not alice itself.
		{"alice.**", "alice", false},
	}
	for _, c := range cases {
		if got := Match(c.sel, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.sel, c.path, got, c.want)
		}
	}
}

func TestMatch_RootWildcard(t *testing.T) {
	for _, path := range []string{"room", "alice.inbox", "team.sub.pipe"} {
		if !Match("**", path) {
			t.Errorf(`Match("**", %q) = false — ** must match every path`, path)
		}
	}
}

// A malformed selector must match nothing rather than everything. This
// is the fail-closed guard: a bad row in acl_grants should grant
// nothing, never the whole account.
func TestMatch_MalformedSelectorMatchesNothing(t *testing.T) {
	// Positive control, as above.
	if !Match("alice.inbox", "alice.inbox") {
		t.Fatal("control: a well-formed selector must match its own path")
	}
	for _, sel := range []Selector{"", ".", "alice.", ".inbox", "alice..inbox"} {
		for _, path := range []string{"alice.inbox", "room", ""} {
			if Match(sel, path) {
				t.Errorf("Match(%q, %q) = true — malformed selectors must match nothing", sel, path)
			}
		}
	}
}
