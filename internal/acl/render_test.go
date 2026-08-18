package acl

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ACL Phase 2 — the three visibility surfaces.
//
//	by-pipe       `ppz pipe acl ls <pipe>`        who can touch this?
//	by-principal  `ppz acl ls --principal <p>`    what can this agent reach?
//	self          `ppz acl whoami [<pipe>]`       what can I do, and why not?
//
// Renderers live here rather than in internal/cli or internal/cliproto
// so they stay pure and table-testable, and so the GUI can render the
// same rows without duplicating the provenance vocabulary.

func mustRender(t *testing.T, fn func(*bytes.Buffer)) string {
	t.Helper()
	var b bytes.Buffer
	fn(&b)
	return b.String()
}

func TestRenderPipeRoster_ColumnsAndProvenance(t *testing.T) {
	owner := alice()
	bob := Principal{ID: uuid.New(), Name: "bob", OrgRole: OrgMember}
	g := Grant{
		PrincipalID: bob.ID, Principal: "bob",
		Selector: "alice.stdout", Perm: Read, GrantedBy: "alice",
		CreatedAt: time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC),
	}
	rows := []RosterRow{
		{Principal: "alice", Decision: Evaluate(owner, stdoutOf(owner), nil)},
		{Principal: "bob", Decision: Evaluate(bob, stdoutOf(owner), []Grant{g})},
	}

	out := mustRender(t, func(b *bytes.Buffer) { RenderPipeRoster(b, "alice.stdout", rows) })

	for _, want := range []string{"PRINCIPAL", "VIA", "alice", "bob"} {
		if !strings.Contains(out, want) {
			t.Errorf("roster output missing %q:\n%s", want, out)
		}
	}
	// Provenance, not just a tick: the granter and date are the point.
	if !strings.Contains(out, "alice") || !strings.Contains(out, "2026-08-14") {
		t.Errorf("roster output must name the granter and date:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "handle owner") {
		t.Errorf("roster output must explain the owner's access as a default:\n%s", out)
	}
}

// Principals with nothing on the pipe are noise — an org of 200 would
// render 198 empty rows.
func TestRenderPipeRoster_OmitsPrincipalsWithNoAccess(t *testing.T) {
	owner := alice()
	carol := Principal{ID: uuid.New(), Name: "carol", OrgRole: OrgMember}
	rows := []RosterRow{
		{Principal: "alice", Decision: Evaluate(owner, stdoutOf(owner), nil)},
		{Principal: "carol", Decision: Evaluate(carol, stdoutOf(owner), nil)},
	}

	out := mustRender(t, func(b *bytes.Buffer) { RenderPipeRoster(b, "alice.stdout", rows) })

	// Positive control first: an empty render would otherwise satisfy
	// the omission assertion without rendering anything at all.
	if !strings.Contains(out, "alice") {
		t.Fatalf("alice holds admin and must appear:\n%s", out)
	}
	if strings.Contains(out, "carol") {
		t.Errorf("carol holds nothing and must not appear:\n%s", out)
	}
}

// The by-principal view has to show derived defaults alongside stored
// grants. Showing only grants reads as "this agent can reach nothing",
// which is the exact misreading this whole surface exists to prevent.
func TestRenderPrincipalGrants_MixesDefaultsAndGrants(t *testing.T) {
	owner := alice()
	bot := Principal{ID: uuid.New(), Name: "builder-bot", OrgRole: OrgMember}
	granted := Grant{
		PrincipalID: bot.ID, Principal: "builder-bot",
		Selector: "ops.deploy-log", Perm: Read | Write, GrantedBy: "foo",
		CreatedAt: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
	}
	rows := []PrincipalRow{
		{Pipe: "alice.inbox", Decision: Evaluate(bot, inboxOf(owner), nil)},
		{Pipe: "ops.deploy-log", Decision: Evaluate(bot,
			Subject{Path: "ops.deploy-log", Collar: "ops", Name: "deploy-log", Owner: uuid.New()},
			[]Grant{granted})},
	}

	out := mustRender(t, func(b *bytes.Buffer) { RenderPrincipalGrants(b, "builder-bot", rows) })

	if !strings.Contains(out, "alice.inbox") {
		t.Errorf("default-derived access missing from the by-principal view:\n%s", out)
	}
	if !strings.Contains(out, "ops.deploy-log") {
		t.Errorf("granted access missing from the by-principal view:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "default") {
		t.Errorf("derived rows must be labelled as defaults so they are distinguishable from grants:\n%s", out)
	}
}

func TestRenderWhoami_ExplainsDenial(t *testing.T) {
	owner := alice()
	bob := Principal{ID: uuid.New(), Name: "bob", OrgRole: OrgMember}
	v := WhoamiView{
		Pipe:      "alice.stdout",
		Principal: "bob",
		Decision:  Evaluate(bob, stdoutOf(owner), nil),
	}

	out := mustRender(t, func(b *bytes.Buffer) { RenderWhoami(b, v) })

	for _, want := range []string{"read", "write", "admin"} {
		if !strings.Contains(strings.ToLower(out), want) {
			t.Errorf("whoami must report every capability, missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(strings.ToLower(out), "owner-only") {
		t.Errorf("whoami must state WHY the capability is denied:\n%s", out)
	}
}

// A denial that doesn't say how to fix it forces the agent to guess.
// The command and the set of principals able to run it are the payload.
func TestRenderWhoami_PrintsRemediationCommand(t *testing.T) {
	owner := alice()
	bob := Principal{ID: uuid.New(), Name: "bob", OrgRole: OrgMember}
	v := WhoamiView{
		Pipe:      "alice.stdout",
		Principal: "bob",
		Decision:  Evaluate(bob, stdoutOf(owner), nil),
		Remediation: &Remediation{
			Command:    "ppz pipe acl grant alice.stdout bob write",
			RunnableBy: []string{"alice (handle owner)", "foo (org owner)"},
		},
	}

	out := mustRender(t, func(b *bytes.Buffer) { RenderWhoami(b, v) })

	if !strings.Contains(out, "ppz pipe acl grant alice.stdout bob write") {
		t.Errorf("whoami must print the exact remediation command:\n%s", out)
	}
}

func TestRenderWhoami_ListsWhoCanRunIt(t *testing.T) {
	owner := alice()
	bob := Principal{ID: uuid.New(), Name: "bob", OrgRole: OrgMember}
	v := WhoamiView{
		Pipe:      "alice.stdout",
		Principal: "bob",
		Decision:  Evaluate(bob, stdoutOf(owner), nil),
		Remediation: &Remediation{
			Command:    "ppz pipe acl grant alice.stdout bob write",
			RunnableBy: []string{"alice (handle owner)", "foo (org owner)"},
		},
	}

	out := mustRender(t, func(b *bytes.Buffer) { RenderWhoami(b, v) })

	if !strings.Contains(out, "alice (handle owner)") || !strings.Contains(out, "foo (org owner)") {
		t.Errorf("whoami must name the principals able to run the remediation:\n%s", out)
	}
}

func TestRenderWhoami_NoRemediationWhenAlreadyPermitted(t *testing.T) {
	owner := alice()
	v := WhoamiView{
		Pipe:      "alice.stdout",
		Principal: "alice",
		Decision:  Evaluate(owner, stdoutOf(owner), nil),
	}

	out := mustRender(t, func(b *bytes.Buffer) { RenderWhoami(b, v) })

	// Positive control: an empty render trivially contains no
	// remediation, so prove the view was actually rendered first.
	if !strings.Contains(out, "alice.stdout") {
		t.Fatalf("whoami rendered nothing:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "handle owner") {
		t.Fatalf("whoami must show why access is held:\n%s", out)
	}
	if strings.Contains(out, "ppz pipe acl grant") {
		t.Errorf("no remediation should be offered when everything is already permitted:\n%s", out)
	}
}

// Agents parse --json. A table scraped by regex is a bug waiting to
// happen, so the JSON shape is pinned by key rather than left to the
// struct definition drifting.
func TestRender_JSONShapeStable(t *testing.T) {
	owner := alice()
	bob := Principal{ID: uuid.New(), Name: "bob", OrgRole: OrgMember}
	dec := Evaluate(bob, inboxOf(owner), nil)

	t.Run("roster", func(t *testing.T) {
		raw, err := json.Marshal([]RosterRow{{Principal: "bob", Decision: dec}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		assertJSONKeys(t, raw, "principal", "read", "write", "admin", "via")
	})

	t.Run("principal", func(t *testing.T) {
		raw, err := json.Marshal([]PrincipalRow{{Pipe: "alice.inbox", Decision: dec}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		assertJSONKeys(t, raw, "pipe", "read", "write", "admin", "via")
	})

	t.Run("whoami", func(t *testing.T) {
		raw, err := json.Marshal(WhoamiView{Pipe: "alice.stdout", Principal: "bob", Decision: dec})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		assertJSONKeys(t, raw, "pipe", "principal", "read", "write", "admin")
	})
}

func assertJSONKeys(t *testing.T, raw []byte, keys ...string) {
	t.Helper()
	s := string(raw)
	for _, k := range keys {
		if !strings.Contains(s, `"`+k+`"`) {
			t.Errorf("JSON missing key %q:\n%s", k, s)
		}
	}
}
