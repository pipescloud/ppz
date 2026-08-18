package server

// Owner-only gates on destructive org operations. Used by the
// session-authed GUI routes; pairs with the requireSession middleware.

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
)

// handleGUIRevokeKey is the session-authed counterpart of the existing
// /api/v1/keys/{id}/revoke. Only the org owner can revoke keys.
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
	if err := db.RevokeAPIKey(r.Context(), s.Pool, keyID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}
	browserSubmit(w, r)
}
