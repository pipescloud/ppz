package acl

import (
	"testing"

	"github.com/google/uuid"
)

// ACL Phase 2 — the derived default table.
//
// The collar is the ownership boundary: everything under a handle
// belongs to that handle's principal. The two exceptions are the pipes
// whose entire purpose is cross-principal traffic, and they are duals
// of each other — inbox takes writes from anyone and is read only by
// its owner; heartbeat is written only by its owner and read by
// everyone. Uncollared pipes are shared org space.
//
// Nothing here is stored. Deriving these rather than seeding an
// "@everyone gets everything" row is what removes the need for deny
// rules and precedence tiers entirely: every stored grant is an allow
// that widens a default.
//
//	<handle>.inbox                            read: owner    write: everyone
//	<handle>.heartbeat                        read: everyone write: owner
//	<handle>.stdin|stdout|stdctrl|system      read: owner    write: owner
//	<handle>.<user-created>                   read: owner    write: owner
//	uncollared <manifold>.<name>              read: everyone write: everyone

func collared(owner Principal, name string) Subject {
	return Subject{Path: owner.Name + "." + name, Collar: owner.Name, Name: name, Owner: owner.ID}
}

func stranger() Principal {
	return Principal{ID: uuid.New(), Name: "bob", OrgRole: OrgMember}
}

// The drop-box: anyone may send to alice, only alice may read what
// arrived. This is the case that forced read and write apart.
func TestDefaults_InboxIsWriteOpenReadClosed(t *testing.T) {
	owner, bob := alice(), stranger()
	d := Evaluate(bob, collared(owner, "inbox"), nil)

	if !d.Has(Write) {
		t.Error("everyone must be able to write to a handle's inbox")
	}
	if d.Has(Read) {
		t.Error("only the handle owner may read an inbox")
	}
	if d.Has(Admin) {
		t.Error("a stranger must not administer another handle's inbox")
	}
}

func TestDefaults_InboxReadableByHandleOwner(t *testing.T) {
	owner := alice()
	d := Evaluate(owner, collared(owner, "inbox"), nil)
	for _, p := range []Perm{Read, Write, Admin} {
		if !d.Has(p) {
			t.Errorf("handle owner missing %v on their own inbox", p)
		}
	}
}

// Presence is the dual of inbox: readable org-wide so `ppz who` works,
// writable only by the handle it describes so liveness can't be forged.
func TestDefaults_HeartbeatIsReadOpenWriteClosed(t *testing.T) {
	owner, bob := alice(), stranger()
	d := Evaluate(bob, collared(owner, "heartbeat"), nil)

	if !d.Has(Read) {
		t.Error("everyone must be able to read a handle's heartbeat (ppz who)")
	}
	if d.Has(Write) {
		t.Error("only the handle owner may publish its own heartbeat")
	}
}

func TestDefaults_StdioIsOwnerOnly(t *testing.T) {
	owner, bob := alice(), stranger()
	for _, name := range []string{"stdin", "stdout", "stdctrl", "system"} {
		d := Evaluate(bob, collared(owner, name), nil)
		if d.Perm != 0 {
			t.Errorf("%s: stranger holds %v, want nothing — terminal sharing is opt-in", name, d.Perm)
		}
		od := Evaluate(owner, collared(owner, name), nil)
		if !od.Has(Read) || !od.Has(Write) {
			t.Errorf("%s: handle owner must hold read+write on their own stdio", name)
		}
	}
}

// Follows the collar rule with no exception: anything under `alice.` is
// alice's until she grants outward.
func TestDefaults_UserCreatedCollaredIsOwnerOnly(t *testing.T) {
	owner, bob := alice(), stranger()
	// Positive control: the owner must hold it, or "nobody holds
	// anything" would satisfy the assertion below.
	if od := Evaluate(owner, collared(owner, "notes"), nil); !od.Has(Read) || !od.Has(Write) {
		t.Fatalf("control: handle owner holds %v on their own notes, want read+write", od.Perm)
	}
	d := Evaluate(bob, collared(owner, "notes"), nil)
	if d.Perm != 0 {
		t.Errorf("stranger holds %v on alice.notes, want nothing", d.Perm)
	}
}

// Uncollared pipes are shared org space — the multi-agent commons.
func TestDefaults_UncollaredIsOpenToMembers(t *testing.T) {
	bob := stranger()
	d := Evaluate(bob, Subject{Path: "room", Collar: "", Name: "room"}, nil)
	if !d.Has(Read) || !d.Has(Write) {
		t.Errorf("uncollared pipe: member holds %v, want read+write", d.Perm)
	}
	if d.Has(Admin) {
		t.Error("a plain member must not administer an uncollared pipe by default")
	}
}

func TestDefaults_UncollaredInNamespaceIsOpenToMembers(t *testing.T) {
	bob := stranger()
	d := Evaluate(bob, Subject{Path: "team.room", Collar: "", Name: "room"}, nil)
	if !d.Has(Read) || !d.Has(Write) {
		t.Errorf("namespaced uncollared pipe: member holds %v, want read+write", d.Perm)
	}
}

// A non-member of the org gets nothing anywhere — the account boundary
// still does the tenancy work, and defaults are scoped inside it.
func TestDefaults_NonMemberGetsNothing(t *testing.T) {
	owner := alice()
	outsider := Principal{ID: uuid.New(), Name: "eve", OrgRole: OrgNone}
	member := stranger()
	for _, s := range []Subject{
		collared(owner, "inbox"),
		collared(owner, "heartbeat"),
		{Path: "room", Collar: "", Name: "room"},
	} {
		// Positive control per subject: a member holds something on
		// each of these by default, so an evaluator that grants
		// nothing at all cannot satisfy the assertion below.
		if md := Evaluate(member, s, nil); md.Perm == 0 {
			t.Fatalf("control: %s should grant a member something by default", s.Path)
		}
		if d := Evaluate(outsider, s, nil); d.Perm != 0 {
			t.Errorf("%s: non-member holds %v, want nothing", s.Path, d.Perm)
		}
	}
}
