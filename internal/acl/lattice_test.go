package acl

import (
	"testing"

	"github.com/google/uuid"
)

// ACL Phase 2 — the permission lattice.
//
//	read and write are INDEPENDENT — neither implies the other
//	admin implies both, plus managing the pipe's ACL/retention/existence
//
// The independence is not a nicety: `<handle>.inbox` needs write
// without read, and it is enforceable rather than merely declarable
// because NATS keeps the two apart. Writes are a bare subject publish
// with the PubAck landing on the caller's own inbox; reads go entirely
// through the JetStream API. Disjoint permission sets.

func TestEvaluate_WriteDoesNotImplyRead(t *testing.T) {
	owner, bob := alice(), stranger()
	g := Grant{PrincipalID: bob.ID, Principal: "bob", Selector: "alice.notes", Perm: Write, GrantedBy: "alice"}

	d := Evaluate(bob, collared(owner, "notes"), []Grant{g})

	if !d.Has(Write) {
		t.Fatal("granted write was not applied")
	}
	if d.Has(Read) {
		t.Error("write must not imply read — the drop-box case depends on it")
	}
}

func TestEvaluate_ReadDoesNotImplyWrite(t *testing.T) {
	owner, bob := alice(), stranger()
	g := Grant{PrincipalID: bob.ID, Principal: "bob", Selector: "alice.stdout", Perm: Read, GrantedBy: "alice"}

	d := Evaluate(bob, collared(owner, "stdout"), []Grant{g})

	if !d.Has(Read) {
		t.Fatal("granted read was not applied")
	}
	if d.Has(Write) {
		t.Error("read must not imply write — an observer of a terminal must not type into it")
	}
}

func TestEvaluate_AdminImpliesReadAndWrite(t *testing.T) {
	owner, bob := alice(), stranger()
	g := Grant{PrincipalID: bob.ID, Principal: "bob", Selector: "alice.notes", Perm: Admin, GrantedBy: "alice"}

	d := Evaluate(bob, collared(owner, "notes"), []Grant{g})

	for _, p := range []Perm{Read, Write, Admin} {
		if !d.Has(p) {
			t.Errorf("admin must imply %v", p)
		}
	}
}

func TestEvaluate_GrantWidensDefault(t *testing.T) {
	owner, bob := alice(), stranger()
	before := Evaluate(bob, collared(owner, "stdout"), nil)
	if before.Perm != 0 {
		t.Fatalf("precondition: bob should hold nothing on alice.stdout, got %v", before.Perm)
	}

	g := Grant{PrincipalID: bob.ID, Principal: "bob", Selector: "alice.stdout", Perm: Read, GrantedBy: "alice"}
	after := Evaluate(bob, collared(owner, "stdout"), []Grant{g})

	if !after.Has(Read) {
		t.Error("a grant must widen the derived default")
	}
}

// Grants accumulate — two rows for the same principal on the same pipe
// compose rather than the last one winning.
func TestEvaluate_GrantsAccumulate(t *testing.T) {
	owner, bob := alice(), stranger()
	grants := []Grant{
		{PrincipalID: bob.ID, Principal: "bob", Selector: "alice.notes", Perm: Read, GrantedBy: "alice"},
		{PrincipalID: bob.ID, Principal: "bob", Selector: "alice.**", Perm: Write, GrantedBy: "alice"},
	}
	d := Evaluate(bob, collared(owner, "notes"), grants)
	if !d.Has(Read) || !d.Has(Write) {
		t.Errorf("grants must compose: got %v, want read+write", d.Perm)
	}
}

// A grant naming @everyone applies to any member of the org.
func TestEvaluate_EveryoneGrantApplies(t *testing.T) {
	owner, bob := alice(), stranger()
	g := Grant{PrincipalID: EveryoneID, Principal: "@everyone", Selector: "alice.notes", Perm: Read, GrantedBy: "alice"}

	d := Evaluate(bob, collared(owner, "notes"), []Grant{g})

	if !d.Has(Read) {
		t.Error("an @everyone grant must apply to any member")
	}
}

// ...but not to a non-member. @everyone means "everyone in this
// account", and the account is the tenancy boundary.
func TestEvaluate_EveryoneGrantExcludesNonMembers(t *testing.T) {
	owner, bob := alice(), stranger()
	outsider := Principal{ID: uuid.New(), Name: "eve", OrgRole: OrgNone}
	g := Grant{PrincipalID: EveryoneID, Principal: "@everyone", Selector: "alice.notes", Perm: Read, GrantedBy: "alice"}

	// Positive control: the grant must actually reach a member.
	if d := Evaluate(bob, collared(owner, "notes"), []Grant{g}); !d.Has(Read) {
		t.Fatal("control: an @everyone grant must reach a member")
	}
	if d := Evaluate(outsider, collared(owner, "notes"), []Grant{g}); d.Perm != 0 {
		t.Errorf("@everyone must not reach a non-member; got %v", d.Perm)
	}
}

// A grant addressed to someone else must never leak.
func TestEvaluate_GrantForAnotherPrincipalIgnored(t *testing.T) {
	owner, bob := alice(), stranger()
	carolID := uuid.New()
	carol := Principal{ID: carolID, Name: "carol", OrgRole: OrgMember}
	g := Grant{PrincipalID: carolID, Principal: "carol", Selector: "alice.stdout", Perm: Read, GrantedBy: "alice"}

	// Positive control: the grant must actually work for carol.
	if d := Evaluate(carol, collared(owner, "stdout"), []Grant{g}); !d.Has(Read) {
		t.Fatal("control: the grant must confer read on carol")
	}
	if d := Evaluate(bob, collared(owner, "stdout"), []Grant{g}); d.Has(Read) {
		t.Error("a grant naming carol must not confer access on bob")
	}
}

// A grant whose selector doesn't match must never leak either.
func TestEvaluate_NonMatchingSelectorIgnored(t *testing.T) {
	owner, bob := alice(), stranger()
	g := Grant{PrincipalID: bob.ID, Principal: "bob", Selector: "alice.stdin", Perm: Read, GrantedBy: "alice"}

	// Positive control: the same grant on its own pipe must work.
	if d := Evaluate(bob, collared(owner, "stdin"), []Grant{g}); !d.Has(Read) {
		t.Fatal("control: the grant must confer read on alice.stdin")
	}
	if d := Evaluate(bob, collared(owner, "stdout"), []Grant{g}); d.Has(Read) {
		t.Error("a grant on alice.stdin must not confer access to alice.stdout")
	}
}

// Org owner and admin hold admin everywhere, computed rather than
// stored — so no revoke can lock an owner out of their own org.
func TestEvaluate_OrgOwnerAndAdminAlwaysAdmin(t *testing.T) {
	owner := alice()
	for _, role := range []OrgRole{OrgOwner, OrgAdmin} {
		p := Principal{ID: uuid.New(), Name: "foo", OrgRole: role}
		for _, name := range []string{"stdout", "inbox", "notes", "heartbeat"} {
			d := Evaluate(p, collared(owner, name), nil)
			for _, perm := range []Perm{Read, Write, Admin} {
				if !d.Has(perm) {
					t.Errorf("org %s missing %v on alice.%s", role, perm, name)
				}
			}
		}
	}
}

// @everyone is a fixed UUID seeded like the existing 'unauthenticated'
// placeholder, so the FK and the uniqueness constraint on acl_grants
// stay honest. It lives here rather than in internal/db because
// internal/acl is the pure package both the evaluator and the storage
// layer agree through — one definition, no drift.
func TestEveryoneID_IsStableAndNotTheNilSentinel(t *testing.T) {
	if EveryoneID == uuid.Nil {
		// uuid.Nil means "unauthenticated" throughout the codebase. If
		// @everyone equalled it, an anonymous caller would match every
		// org-wide grant.
		t.Fatal("@everyone must never collide with the unauthenticated sentinel")
	}
	if EveryoneID.String() != "00000000-0000-0000-0000-000000000002" {
		t.Errorf("EveryoneID = %s, want the seeded fixed UUID", EveryoneID)
	}
}
