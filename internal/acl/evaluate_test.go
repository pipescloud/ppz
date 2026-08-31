package acl

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// ACL Phase 2 — Evaluate returns provenance, not a bare bitset.
//
// With defaults derived from (collar, pipe name) rather than stored,
// most access has NO row behind it. A surface that renders acl_grants
// shows an almost-empty table and tells the operator that nobody can
// reach alice.inbox — when in fact every member can write to it.
//
// Decision.Why is what makes the difference visible, so it is pinned as
// hard as Decision.Perm. The JWT compiler consumes Perm; every human-
// and agent-facing surface consumes Why.

func alice() Principal {
	return Principal{ID: uuid.New(), Name: "alice", OrgRole: OrgMember}
}

// stdoutOf builds the owner-only subject <handle>.stdout.
func stdoutOf(owner Principal) Subject {
	return Subject{Path: owner.Name + ".stdout", Collar: owner.Name, Name: "stdout", Owner: owner.ID}
}

// inboxOf builds the write-open subject <handle>.inbox.
func inboxOf(owner Principal) Subject {
	return Subject{Path: owner.Name + ".inbox", Collar: owner.Name, Name: "inbox", Owner: owner.ID}
}

// Access that exists purely because of the default table still has to
// explain itself — this is the case the raw grant table cannot show.
func TestEvaluate_ReportsDefaultAsReason(t *testing.T) {
	owner := alice()
	bob := Principal{ID: uuid.New(), Name: "bob", OrgRole: OrgMember}

	got := Evaluate(bob, inboxOf(owner), nil)

	if !got.Has(Write) {
		t.Fatal("bob must be able to write to alice.inbox by default")
	}
	r := got.Reason(Write)
	if r.Kind != ReasonDefault {
		t.Errorf("Write reason kind = %q, want %q", r.Kind, ReasonDefault)
	}
	if r.Detail == "" {
		t.Error("default reason must carry a human-readable Detail — it is the whole UI string")
	}
	if r.Grant != nil {
		t.Error("a derived default must not claim a granting row")
	}
}

// A granted capability carries the row, so the UI can render
// "grant by alice · 2026-08-14".
func TestEvaluate_ReportsGrantAsReason(t *testing.T) {
	owner := alice()
	bob := Principal{ID: uuid.New(), Name: "bob", OrgRole: OrgMember}
	when := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	g := Grant{
		PrincipalID: bob.ID,
		Principal:   "bob",
		Selector:    "alice.stdout",
		Perm:        Read,
		GrantedBy:   "alice",
		CreatedAt:   when,
	}

	got := Evaluate(bob, stdoutOf(owner), []Grant{g})

	if !got.Has(Read) {
		t.Fatal("granted read was not applied")
	}
	r := got.Reason(Read)
	if r.Kind != ReasonGrant {
		t.Fatalf("Read reason kind = %q, want %q", r.Kind, ReasonGrant)
	}
	if r.Grant == nil {
		t.Fatal("grant reason must carry the row so the UI can name the granter and date")
	}
	if r.Grant.GrantedBy != "alice" || !r.Grant.CreatedAt.Equal(when) {
		t.Errorf("grant provenance = %s/%v, want alice/%v", r.Grant.GrantedBy, r.Grant.CreatedAt, when)
	}
}

func TestEvaluate_ReportsHandleOwnerAsReason(t *testing.T) {
	owner := alice()

	got := Evaluate(owner, stdoutOf(owner), nil)

	for _, p := range []Perm{Read, Write, Admin} {
		if !got.Has(p) {
			t.Fatalf("handle owner missing %v on their own stdout", p)
		}
		if k := got.Reason(p).Kind; k != ReasonHandleOwner {
			t.Errorf("%v reason kind = %q, want %q", p, k, ReasonHandleOwner)
		}
	}
}

func TestEvaluate_ReportsOrgRoleAsReason(t *testing.T) {
	owner := alice()
	admin := Principal{ID: uuid.New(), Name: "foo", OrgRole: OrgAdmin}

	got := Evaluate(admin, stdoutOf(owner), nil)

	if !got.Has(Admin) {
		t.Fatal("org admin must hold admin everywhere")
	}
	if k := got.Reason(Admin).Kind; k != ReasonOrgRole {
		t.Errorf("Admin reason kind = %q, want %q", k, ReasonOrgRole)
	}
}

// When a default and a grant would both confer the capability, the
// grant is the reason reported. Otherwise revoking the row leaves the
// UI unchanged and the operator concludes the revoke failed.
func TestEvaluate_GrantOverDefaultReportsGrant(t *testing.T) {
	owner := alice()
	bob := Principal{ID: uuid.New(), Name: "bob", OrgRole: OrgMember}
	g := Grant{
		PrincipalID: bob.ID, Principal: "bob",
		Selector: "alice.inbox", Perm: Write,
		GrantedBy: "alice", CreatedAt: time.Now(),
	}

	got := Evaluate(bob, inboxOf(owner), []Grant{g})

	if k := got.Reason(Write).Kind; k != ReasonGrant {
		t.Errorf("Write reason kind = %q, want %q — the explicit row must win over the identical default", k, ReasonGrant)
	}
}

// Property: no capability is ever granted without provenance. Runs over
// every combination of subject shape, org role and grant set, so a
// future code path that returns a bare Perm is caught here rather than
// as a blank VIA column in production.
func TestEvaluate_EveryPermittedCapabilityHasAReason(t *testing.T) {
	owner := alice()
	subjects := []Subject{
		inboxOf(owner),
		stdoutOf(owner),
		{Path: owner.Name + ".heartbeat", Collar: owner.Name, Name: "heartbeat", Owner: owner.ID},
		{Path: owner.Name + ".notes", Collar: owner.Name, Name: "notes", Owner: owner.ID},
		{Path: "room", Collar: "", Name: "room"},
	}
	roles := []OrgRole{OrgOwner, OrgAdmin, OrgMember}
	bob := uuid.New()
	grantSets := [][]Grant{
		nil,
		{{PrincipalID: bob, Principal: "bob", Selector: "**", Perm: Read, GrantedBy: "foo"}},
		{{PrincipalID: bob, Principal: "bob", Selector: "alice.**", Perm: Admin, GrantedBy: "alice"}},
	}

	checked := 0
	for _, s := range subjects {
		for _, role := range roles {
			for _, gs := range grantSets {
				p := Principal{ID: bob, Name: "bob", OrgRole: role}
				d := Evaluate(p, s, gs)
				for _, perm := range []Perm{Read, Write, Admin} {
					if !d.Has(perm) {
						continue
					}
					checked++
					r := d.Reason(perm)
					if r.Kind == "" || r.Kind == ReasonNone {
						t.Errorf("subject=%s role=%s perm=%v granted with no reason", s.Path, role, perm)
					}
					if r.Detail == "" {
						t.Errorf("subject=%s role=%s perm=%v reason has empty Detail", s.Path, role, perm)
					}
				}
			}
		}
	}
	// Guard against the whole sweep passing because nothing was
	// granted: an org owner over 5 subjects alone owes 15 capabilities.
	if checked < 15 {
		t.Fatalf("only %d capabilities were granted across the sweep — the fixture is not exercising Evaluate", checked)
	}
}

// Denials need provenance too — the ✗ rows in `ppz acl whoami` are the
// reason anyone runs it.
func TestEvaluate_DeniedCapabilityExplainsWhy(t *testing.T) {
	owner := alice()
	bob := Principal{ID: uuid.New(), Name: "bob", OrgRole: OrgMember}

	got := Evaluate(bob, stdoutOf(owner), nil)

	if got.Has(Read) {
		t.Fatal("bob must not read alice.stdout by default")
	}
	r := got.Reason(Read)
	if r.Kind != ReasonNone {
		t.Errorf("denied Read reason kind = %q, want %q", r.Kind, ReasonNone)
	}
	if r.Detail == "" {
		t.Error("denial must explain itself — this is the string whoami prints after ✗")
	}
}
