package server

// Role checks — used by the gates on destructive org operations
// (revoke key, remove member, transfer ownership) and, from ACL Phase 2,
// by the ACL evaluator.

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pipescloud/ppz/internal/acl"
)

// OrgRole is the role the user has in an org. "" = not a member.
type OrgRole string

const (
	OrgRoleOwner OrgRole = "owner"
	// OrgRoleAdmin (ACL Phase 1) can do everything an owner can except
	// transfer ownership and change roles. Deliberately below owner:
	// widening the org gates to admin must not let an admin promote
	// themselves, or the owner tier is decorative.
	OrgRoleAdmin  OrgRole = "admin"
	OrgRoleMember OrgRole = "member"
	OrgRoleNone   OrgRole = ""
)

// AtLeast reports whether this role satisfies a gate requiring `min`.
// Unknown values fail closed. Ordering is delegated to internal/acl so
// the HTTP gates and the ACL evaluator cannot disagree.
func (r OrgRole) AtLeast(min OrgRole) bool {
	return acl.OrgRole(r).AtLeast(acl.OrgRole(min))
}

// CanAdministerOrg reports whether the role carries implicit admin on
// every pipe in the account.
func (r OrgRole) CanAdministerOrg() bool {
	return acl.OrgRole(r).CanAdministerOrg()
}

// RoleInOrg returns the calling user's role in the given org.
//
//   - OrgRoleOwner  if users.id == accounts.owner_user_id
//   - OrgRoleAdmin  if account_members.role = 'admin'
//   - OrgRoleMember if listed in account_members otherwise
//   - OrgRoleNone   otherwise
//
// accounts.owner_user_id remains the authority for ownership; the role
// column only distinguishes admin from member. Checking the owner
// column first means an org can never be left without an owner by a
// bad row in account_members.
func (s *Server) RoleInOrg(ctx context.Context, userID, accountID uuid.UUID) (OrgRole, error) {
	if userID == uuid.Nil || accountID == uuid.Nil {
		return OrgRoleNone, nil
	}
	var ownerID uuid.UUID
	err := s.Pool.QueryRow(ctx,
		`SELECT owner_user_id FROM accounts WHERE id = $1`, accountID).
		Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrgRoleNone, nil
	}
	if err != nil {
		return OrgRoleNone, err
	}
	if ownerID == userID {
		return OrgRoleOwner, nil
	}
	var role string
	err = s.Pool.QueryRow(ctx,
		`SELECT role FROM account_members
		  WHERE account_id = $1 AND user_id = $2`, accountID, userID).
		Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrgRoleNone, nil
	}
	if err != nil {
		return OrgRoleNone, err
	}
	switch OrgRole(role) {
	case OrgRoleAdmin:
		return OrgRoleAdmin, nil
	case OrgRoleOwner:
		// A stale 'owner' role row on someone who is not
		// accounts.owner_user_id. Treat as admin rather than trusting
		// it — the accounts column is the authority.
		return OrgRoleAdmin, nil
	default:
		return OrgRoleMember, nil
	}
}
