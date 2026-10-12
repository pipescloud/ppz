package server

// Owner-only gates on destructive org operations. Used by the
// session-authed GUI routes; pairs with the requireSession middleware.

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pipescloud/ppz/internal/db"
)

// handleGUIRevokeKey revokes one of the org's API keys.
//
//	owner / admin → any of the org's keys
//	member        → only keys they created (so a member who leaks a key
//	                can kill it); anyone else's is 403
//	non-member    → 404 (orgSession)
//	other org's key → 404
func (s *Server) handleGUIRevokeKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	a, ok := s.orgAccessFor(ctx, w, r)
	if !ok {
		return
	}
	keyID, err := uuid.Parse(r.PathValue("kid"))
	if err != nil {
		http.Error(w, "invalid key id", http.StatusBadRequest)
		return
	}
	if !a.role.CanAdministerOrg() {
		creator, err := db.APIKeyCreator(ctx, s.Pool, a.org.ID, keyID)
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if creator != UserIDFromCtx(r.Context()) {
			http.Error(w, "org admin or key creator only", http.StatusForbidden)
			return
		}
	}
	if err := db.RevokeAPIKey(ctx, s.Pool, a.org.ID, keyID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}
	browserSubmit(w, r)
}

// orgAccess is the org named by {id} plus the session user's role in it,
// resolved once by orgSession and carried on the request context.
type orgAccess struct {
	org  db.Account
	role OrgRole
}

type ctxKeyOrgAccessType struct{}

var ctxKeyOrgAccess ctxKeyOrgAccessType

// orgSession wraps every route under /orgs/{id}: session auth plus org
// membership. requireSession alone only proves the caller is logged in,
// not that the org is theirs; registering org routes through this (see
// TestOrgRoutesUseOrgSession) makes the membership check structural
// rather than something each handler has to remember. Finer gates
// (admin, owner) stay in the handlers via resolveAdminOrg / orgRole.
func (s *Server) orgSession(next http.HandlerFunc) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request) {
		// Bounded lookup context only: next may be a long-lived
		// WebSocket and must keep the request's own context.
		ctx, cancel := withTimeout(r)
		a, ok := s.lookupOrgAccess(ctx, w, r)
		cancel()
		if !ok {
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKeyOrgAccess, a)))
	})
}

// lookupOrgAccess resolves {id} and the caller's role. A missing org and
// a non-member both get 404, so org existence doesn't leak to outsiders;
// a lookup failure is a 500, not a misleading "not found".
func (s *Server) lookupOrgAccess(ctx context.Context, w http.ResponseWriter, r *http.Request) (orgAccess, bool) {
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "org not found", http.StatusNotFound)
		return orgAccess{}, false
	}
	if err != nil {
		http.Error(w, "org lookup: "+err.Error(), http.StatusInternalServerError)
		return orgAccess{}, false
	}
	role, err := s.RoleInOrg(ctx, UserIDFromCtx(r.Context()), org.ID)
	if err != nil {
		http.Error(w, "role check: "+err.Error(), http.StatusInternalServerError)
		return orgAccess{}, false
	}
	if role == OrgRoleNone {
		http.Error(w, "org not found", http.StatusNotFound)
		return orgAccess{}, false
	}
	return orgAccess{org: org, role: role}, true
}

// orgAccessFor returns the access orgSession resolved, or resolves it if
// the handler was reached some other way (tests, a route registered
// without orgSession).
func (s *Server) orgAccessFor(ctx context.Context, w http.ResponseWriter, r *http.Request) (orgAccess, bool) {
	if a, ok := r.Context().Value(ctxKeyOrgAccess).(orgAccess); ok {
		return a, true
	}
	return s.lookupOrgAccess(ctx, w, r)
}

// resolveMemberOrg returns the org for a route any member may use.
func (s *Server) resolveMemberOrg(ctx context.Context, w http.ResponseWriter, r *http.Request) (db.Account, bool) {
	a, ok := s.orgAccessFor(ctx, w, r)
	return a.org, ok
}

// resolveAdminOrg returns the org for an org-administration route:
// non-member → 404, member without admin rights → 403.
func (s *Server) resolveAdminOrg(ctx context.Context, w http.ResponseWriter, r *http.Request) (db.Account, bool) {
	a, ok := s.orgAccessFor(ctx, w, r)
	if !ok {
		return db.Account{}, false
	}
	if !a.role.CanAdministerOrg() {
		http.Error(w, "org admin only", http.StatusForbidden)
		return db.Account{}, false
	}
	return a.org, true
}
