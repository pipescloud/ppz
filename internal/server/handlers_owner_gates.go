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

	// Read the key before revoking it. Two things need it: the audit row
	// names the key by LABEL (an id tells a reader nothing), and the row
	// has to be filed against the key's own account.
	//
	// Which is also how the org check below became possible. The gate
	// above proves the caller administers the org in the PATH, and
	// nothing until now proved the key belonged to it — so an owner of
	// one org could revoke another org's key by id. 404, not 403: a
	// caller who can't see the key shouldn't learn it exists.
	key, err := db.GetAPIKey(r.Context(), s.Pool, keyID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}
	if key.AccountID != org.ID {
		http.NotFound(w, r)
		return
	}
	alreadyRevoked := key.Revoked()

	if err := db.RevokeAPIKey(r.Context(), s.Pool, keyID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}

	// RevokeAPIKey is idempotent, so a re-revoke reaches here having
	// changed nothing. Audit the state change, not the request — the
	// same rule source.promote follows.
	if !alreadyRevoked {
		s.auditOrg(r.Context(), org.ID, AuthedCaller{UserID: uid}, db.AuditActionKeyRevoke,
			db.AuditTargetKey, key.Label,
			fieldPayload(map[string]string{"state": "active"}),
			fieldPayload(map[string]string{"state": "revoked"}))
	}
	browserSubmit(w, r)
}
