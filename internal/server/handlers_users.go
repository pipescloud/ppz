package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
)

// handleCreateUser: POST /users (form: username, email, mode).
// Test-only (404 unless DevLogin): production users are created by the
// GitHub OAuth callback; internal-mode users are created here for e2e.
//
// Redirects to /users on success so the new row is visible. API
// callers (no Referer) get a plain 303 to the same place.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	// Dev/test only: creating arbitrary user rows (any username, email
	// and mode) isn't something a logged-in user should be able to do
	// in production, where users arrive via OAuth.
	if !s.DevLogin {
		http.NotFound(w, r)
		return
	}
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
	org, ok := s.resolveAdminOrg(ctx, w, r)
	if !ok {
		return
	}
	if err := db.AddMember(ctx, s.Pool, org.ID, userID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
	org, ok := s.resolveAdminOrg(ctx, w, r)
	if !ok {
		return
	}
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
	browserSubmit(w, r)
}