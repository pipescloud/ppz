package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
)

// handleCreateUser: POST /users (form: username, email, mode).
// Used by the v1 GUI; v2 replaces this path with the GitHub OAuth
// callback. Internal-mode users continue to be created here for
// e2e + non-OAuth flows.
//
// Redirects to /users on success so the new row is visible. API
// callers (no Referer) get a plain 303 to the same place.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	email := strings.TrimSpace(r.FormValue("email"))
	mode := db.UserMode(strings.TrimSpace(r.FormValue("mode")))
	if username == "" || email == "" {
		http.Error(w, "username and email required", http.StatusBadRequest)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	if _, err := db.InsertUser(ctx, s.Pool, username, email, mode); err != nil {
		if errors.Is(err, db.ErrInvalidUserMode) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	browserSubmit(w, r)
}

// handleAddMember: POST /orgs/{id}/members (form: user_id) — adds the
// user as a non-owner member of the org. Idempotent: re-adding an
// existing member is a no-op + still 303.
func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userID, err := uuid.Parse(strings.TrimSpace(r.FormValue("user_id")))
	if err != nil {
		http.Error(w, "user_id is not a valid uuid", http.StatusBadRequest)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("id"))
	if err != nil {
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	// Read the role first: AddMember is idempotent, so re-adding an
	// existing member changes nothing and must not produce a row. Audit
	// the state change, not the request.
	wasRole, _ := s.RoleInOrg(ctx, userID, org.ID)

	if err := db.AddMember(ctx, s.Pool, org.ID, userID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if wasRole == OrgRoleNone {
		// Read the role back rather than assuming "member" — which tier
		// a new row lands on is the column default's business, and the
		// trail should state what actually happened.
		nowRole, _ := s.RoleInOrg(ctx, userID, org.ID)
		s.auditOrg(ctx, org.ID, AuthedCaller{UserID: UserIDFromCtx(r.Context())},
			db.AuditActionMemberAdd, db.AuditTargetUser,
			auditUsername(ctx, s.Pool, userID), nil, rolePayload(nowRole))
	}
	browserSubmit(w, r)
}

// handleRemoveMember: POST /orgs/{id}/members/{uid}/remove — removes
// the user from the org. Returns 409 when the target is the org's
// owner (transfer-ownership is a v2 feature).
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(r.PathValue("uid"))
	if err != nil {
		http.Error(w, "user_id is not a valid uuid", http.StatusBadRequest)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("id"))
	if err != nil {
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	// Snapshot the tier before it goes — after the DELETE there is
	// nothing left to read it from, and "which role did they hold when
	// we removed them" is the question this row exists to answer.
	wasRole, _ := s.RoleInOrg(ctx, userID, org.ID)
	username := auditUsername(ctx, s.Pool, userID)

	if err := db.RemoveMember(ctx, s.Pool, org.ID, userID); err != nil {
		if errors.Is(err, db.ErrCannotRemoveOwner) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.Is(err, db.ErrNotFound) {
			http.Error(w, "member not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.auditOrg(ctx, org.ID, AuthedCaller{UserID: UserIDFromCtx(r.Context())},
		db.AuditActionMemberRemove, db.AuditTargetUser,
		username, rolePayload(wasRole), nil)
	browserSubmit(w, r)
}
