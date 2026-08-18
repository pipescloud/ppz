package server

import "testing"

// ACL Phase 1 — org roles gain an admin tier.
//
// Today "owner" means users.id == accounts.owner_user_id and everyone
// else in account_members is "member" (role.go). There is no admin
// tier at all, and account_members has no role column — so the ACL
// model's "org owner and admin hold implicit admin on **" has nothing
// to read.
//
// Ordering is pinned as a pure predicate rather than left to switch
// statements at each gate: handlers_owner_gates.go is the only current
// call site, and Phase 2 adds several more that must agree.

func TestOrgRole_Ordering(t *testing.T) {
	cases := []struct {
		role OrgRole
		min  OrgRole
		want bool
	}{
		{OrgRoleOwner, OrgRoleOwner, true},
		{OrgRoleOwner, OrgRoleAdmin, true},
		{OrgRoleOwner, OrgRoleMember, true},

		{OrgRoleAdmin, OrgRoleOwner, false},
		{OrgRoleAdmin, OrgRoleAdmin, true},
		{OrgRoleAdmin, OrgRoleMember, true},

		{OrgRoleMember, OrgRoleOwner, false},
		{OrgRoleMember, OrgRoleAdmin, false},
		{OrgRoleMember, OrgRoleMember, true},

		{OrgRoleNone, OrgRoleOwner, false},
		{OrgRoleNone, OrgRoleAdmin, false},
		{OrgRoleNone, OrgRoleMember, false},
	}
	for _, c := range cases {
		if got := c.role.AtLeast(c.min); got != c.want {
			t.Errorf("OrgRole(%q).AtLeast(%q) = %v, want %v", c.role, c.min, got, c.want)
		}
	}
}

// The predicate the ACL evaluator reads for "implicit admin on **".
func TestOrgRole_CanAdministerOrg(t *testing.T) {
	cases := map[OrgRole]bool{
		OrgRoleOwner:  true,
		OrgRoleAdmin:  true,
		OrgRoleMember: false,
		OrgRoleNone:   false,
	}
	for role, want := range cases {
		if got := role.CanAdministerOrg(); got != want {
			t.Errorf("OrgRole(%q).CanAdministerOrg() = %v, want %v", role, got, want)
		}
	}
}

// An unrecognised role string must not accidentally outrank a member.
// Roles arrive from a DB column with a CHECK constraint, but a
// mis-scanned or future value must fail closed.
func TestOrgRole_UnknownFailsClosed(t *testing.T) {
	// Positive control: predicates that always say no would satisfy
	// both assertions below without implementing any ordering.
	if !OrgRoleOwner.AtLeast(OrgRoleMember) || !OrgRoleOwner.CanAdministerOrg() {
		t.Fatal("control: owner must outrank member and administer the org")
	}
	unknown := OrgRole("superuser")
	if unknown.AtLeast(OrgRoleMember) {
		t.Error("an unrecognised role must not satisfy AtLeast(member)")
	}
	if unknown.CanAdministerOrg() {
		t.Error("an unrecognised role must not administer the org")
	}
}

// Owner remains the only role that can transfer ownership — admin is
// deliberately below it, so widening the gates in Phase 1 must not
// silently widen transfer too.
func TestOrgRole_AdminIsNotOwner(t *testing.T) {
	// Positive control, as above.
	if !OrgRoleAdmin.AtLeast(OrgRoleAdmin) {
		t.Fatal("control: admin must satisfy an admin-level gate")
	}
	if OrgRoleAdmin.AtLeast(OrgRoleOwner) {
		t.Error("admin must not satisfy an owner-only gate (ownership transfer)")
	}
}
