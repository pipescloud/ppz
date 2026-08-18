package acl

import (
	"testing"

	"github.com/google/uuid"
)

// ACL Phase 2 — who may see a pipe's roster.
//
// Locked: any principal holding ANY access on the pipe. Coordination
// needs it (an agent that gets denied has to know who to ask), and it
// leaks little, since handle and pipe names are already org-visible via
// `ppz ls`. A principal with nothing on the pipe gets E_PIPE_FORBIDDEN.
//
// CanSeeRoster takes the Decision rather than the principal so the CLI
// verb, the HTTP handler and the GUI panel all gate on one rule
// evaluated once.

func TestCanSeeRoster_HolderOfAnyPerm(t *testing.T) {
	owner := alice()
	bob := Principal{ID: uuid.New(), Name: "bob", OrgRole: OrgMember}

	cases := []struct {
		name string
		perm Perm
	}{
		{"read-only observer", Read},
		{"write-only sender", Write},
		{"admin", Admin},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := Grant{PrincipalID: bob.ID, Principal: "bob", Selector: "alice.stdout", Perm: c.perm, GrantedBy: "alice"}
			d := Evaluate(bob, stdoutOf(owner), []Grant{g})
			if !CanSeeRoster(d) {
				t.Errorf("principal holding %v cannot see the roster", c.perm)
			}
		})
	}
}

// The write-only case is the one most likely to be got wrong: a sender
// to alice.inbox holds no read, and a naive "can you read it" check
// would hide the roster from every inbox sender in the org.
func TestCanSeeRoster_WriteOnlyPrincipalQualifies(t *testing.T) {
	owner := alice()
	bob := Principal{ID: uuid.New(), Name: "bob", OrgRole: OrgMember}

	d := Evaluate(bob, inboxOf(owner), nil) // write by default, no read

	if d.Has(Read) {
		t.Fatal("precondition: bob must not hold read on alice.inbox")
	}
	if !d.Has(Write) {
		t.Fatal("precondition: bob must hold write on alice.inbox")
	}
	if !CanSeeRoster(d) {
		t.Error("a write-only principal must still see the roster")
	}
}

func TestCanSeeRoster_NoAccessDenied(t *testing.T) {
	owner := alice()
	carol := Principal{ID: uuid.New(), Name: "carol", OrgRole: OrgMember}

	// Positive control: an implementation that always says no would
	// otherwise satisfy the denial assertion vacuously.
	if !CanSeeRoster(Evaluate(owner, stdoutOf(owner), nil)) {
		t.Fatal("control: the handle owner must be able to see the roster")
	}

	d := Evaluate(carol, stdoutOf(owner), nil)

	if d.Perm != 0 {
		t.Fatalf("precondition: carol must hold nothing on alice.stdout, got %v", d.Perm)
	}
	if CanSeeRoster(d) {
		t.Error("a principal with no access on the pipe must not see its roster")
	}
}

func TestCanSeeRoster_OrgAdminAlways(t *testing.T) {
	owner := alice()
	for _, role := range []OrgRole{OrgOwner, OrgAdmin} {
		admin := Principal{ID: uuid.New(), Name: "foo", OrgRole: role}
		d := Evaluate(admin, stdoutOf(owner), nil)
		if !CanSeeRoster(d) {
			t.Errorf("org %s must always see the roster", role)
		}
	}
}
