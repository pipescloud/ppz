package server

// Owner-only gates on destructive org operations. Used by the
// session-authed GUI routes; pairs with the requireSession middleware.

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
)

// handleGUIRevokeKey revokes one of the org's API keys. Owner or admin
// only; a key belonging to another org is a 404.
//
//	owner   → revoke + 303 back to org page (or 200 if no Referer)
//	admin   → same as owner (ACL Phase 1)
//	member  → 403, key untouched
//	non-mem → 404 (don't leak that the org exists)
func (s *Server) handleGUIRevokeKey(w http.ResponseWriter, r *http.Request) {
	uid := UserIDFromCtx(r.Context())
	org, err := resolveOrg(r.Context(), s.Pool, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	role, err := s.RoleInOrg(r.Context(), uid, org.ID)
	if err != nil {
		http.Error(w, "role check: "+err.Error(), 500)
		return
	}
	// Explicit rather than relying on unmatched cases falling out of the
	// switch: OrgRoleAdmin was silently permitted that way, which is the
	// right answer for the wrong reason.
	switch {
	case role == OrgRoleNone:
		http.NotFound(w, r)
		return
	case !role.CanAdministerOrg():
		http.Error(w, "org admin only", http.StatusForbidden)
		return
	}

	keyID, err := uuid.Parse(r.PathValue("kid"))
	if err != nil {
		http.Error(w, "invalid key id", http.StatusBadRequest)
		return
	}
	if err := db.RevokeAPIKey(r.Context(), s.Pool, org.ID, keyID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}
	browserSubmit(w, r)
}

// resolveMemberOrg resolves the org from the URL and confirms the session
// user is a member or owner. Every session route under /orgs/{id} goes
// through this (or resolveAdminOrg): requireSession only proves the caller
// is logged in, not that the org is theirs. A non-member gets 404 (not
// 403) so org existence doesn't leak to outsiders.
func (s *Server) resolveMemberOrg(ctx context.Context, w http.ResponseWriter, r *http.Request) (db.Account, bool) {
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("id"))
	if err != nil {
		http.Error(w, "org not found", 404)
		return db.Account{}, false
	}
	if !db.IsMemberOrOwner(ctx, s.Pool, org.ID, UserIDFromCtx(r.Context())) {
		http.Error(w, "org not found", 404)
		return db.Account{}, false
	}
	return org, true
}

// resolveAdminOrg is resolveMemberOrg for org-administration routes:
// non-member → 404, member without admin rights → 403.
func (s *Server) resolveAdminOrg(ctx context.Context, w http.ResponseWriter, r *http.Request) (db.Account, bool) {
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("id"))
	if err != nil {
		http.Error(w, "org not found", 404)
		return db.Account{}, false
	}
	role, err := s.RoleInOrg(ctx, UserIDFromCtx(r.Context()), org.ID)
	if err != nil {
		http.Error(w, "role check: "+err.Error(), 500)
		return db.Account{}, false
	}
	switch {
	case role == OrgRoleNone:
		http.Error(w, "org not found", 404)
		return db.Account{}, false
	case !role.CanAdministerOrg():
		http.Error(w, "org admin only", http.StatusForbidden)
		return db.Account{}, false
	}
	return org, true
}
