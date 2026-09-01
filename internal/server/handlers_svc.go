package server

// Service-account and member-role routes — ACL Phase 1.
//
// Service accounts are org-admin-managed: creating one mints a
// principal that can hold ACL grants and own handles, so the gate is
// the same tier that manages keys and membership.

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
)

type createServiceRequest struct {
	Name string `json:"name"`
}

type serviceWire struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func serviceToWire(u db.User) serviceWire {
	return serviceWire{ID: u.ID.String(), Name: u.DisplayName()}
}

// resolveCallerOrg finds the org a bearer is acting in and the caller's
// role in it, applying the same `?org=` / default-org resolution as
// requireAPIKey.
func (s *Server) resolveCallerOrg(r *http.Request) (db.Account, OrgRole, error) {
	caller := CallerFromCtx(r.Context())
	if caller.Principal() == uuid.Nil {
		return db.Account{}, OrgRoleNone, errors.New("no principal")
	}
	var org db.Account
	var err error
	if raw := r.URL.Query().Get("org"); raw != "" {
		id, perr := uuid.Parse(raw)
		if perr != nil {
			return db.Account{}, OrgRoleNone, errors.New("org is not a valid uuid")
		}
		org, err = db.GetAccount(r.Context(), s.Pool, id)
	} else if caller.APIKey != nil {
		org, err = db.GetAccount(r.Context(), s.Pool, caller.APIKey.AccountID)
	} else {
		org, err = db.DefaultAccountFor(r.Context(), s.Pool, caller.Principal())
	}
	if err != nil {
		return db.Account{}, OrgRoleNone, err
	}
	role, err := s.RoleInOrg(r.Context(), caller.Principal(), org.ID)
	if err != nil {
		return db.Account{}, OrgRoleNone, err
	}
	return org, role, nil
}

// requireOrgAdmin resolves the caller's org and refuses anyone below
// admin. Non-members get 404 rather than 403 so the org's existence
// isn't leaked.
func (s *Server) requireOrgAdmin(w http.ResponseWriter, r *http.Request) (db.Account, bool) {
	org, role, err := s.resolveCallerOrg(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return db.Account{}, false
	}
	switch {
	case role == OrgRoleNone:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return db.Account{}, false
	case !role.CanAdministerOrg():
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "org admin only"})
		return db.Account{}, false
	}
	return org, true
}

// handleAPICreateService: POST /api/v1/svc
func (s *Server) handleAPICreateService(w http.ResponseWriter, r *http.Request) {
	var req createServiceRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Validate before the org lookup: a name carrying the scope
	// separator could otherwise be stored as another org's scoped
	// username, so it must never survive far enough to be written.
	if _, err := db.ValidateServiceName(req.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	org, ok := s.requireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	u, err := db.InsertServiceAccount(ctx, s.Pool, org.ID, org.Name, req.Name)
	if err != nil {
		if errors.Is(err, db.ErrServiceExists) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.auditOrg(ctx, org.ID, CallerFromCtx(r.Context()), db.AuditActionSvcCreate,
		db.AuditTargetService, u.DisplayName(), nil,
		fieldPayload(map[string]string{"state": "created"}))

	writeJSON(w, http.StatusCreated, map[string]any{"service": serviceToWire(u)})
}

// handleAPIListServices: GET /api/v1/svc
func (s *Server) handleAPIListServices(w http.ResponseWriter, r *http.Request) {
	org, role, err := s.resolveCallerOrg(r)
	if err != nil || role == OrgRoleNone {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	rows, err := db.ListServiceAccounts(ctx, s.Pool, org.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]serviceWire, 0, len(rows))
	for _, u := range rows {
		out = append(out, serviceToWire(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": out})
}

// handleAPIDeleteService: DELETE /api/v1/svc/{name}
func (s *Server) handleAPIDeleteService(w http.ResponseWriter, r *http.Request) {
	org, ok := s.requireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	if err := db.DeleteServiceAccount(ctx, s.Pool, org.ID, org.Name, r.PathValue("name")); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "service account not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Targeted by the BARE name, matching every other surface: usernames
	// are stored scoped as "<org>/<name>" only because users.username is
	// globally unique.
	s.auditOrg(ctx, org.ID, CallerFromCtx(r.Context()), db.AuditActionSvcDestroy,
		db.AuditTargetService, r.PathValue("name"),
		fieldPayload(map[string]string{"state": "created"}), nil)

	w.WriteHeader(http.StatusNoContent)
}

// handleAPIMintServiceKey: POST /api/v1/svc/{name}/keys
//
// The minted key is created_by the caller and acts_as the service. That
// divergence is the point: the agent's work is its own, and an ACL
// grant naming the service governs it rather than the human's rights
// leaking through.
func (s *Server) handleAPIMintServiceKey(w http.ResponseWriter, r *http.Request) {
	org, ok := s.requireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	svc, err := db.GetServiceAccount(ctx, s.Pool, org.ID, org.Name, r.PathValue("name"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "service account not found"})
		return
	}
	caller := CallerFromCtx(r.Context())
	key, plaintext, err := db.InsertAPIKeyAs(ctx, s.Pool, org.ID, caller.Principal(), svc.ID, svc.DisplayName())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Attributed to the human who minted it, targeted at the bot it acts
	// as. That split is the whole reason this row matters: from here on
	// the key's own work attributes to the service, so this is the last
	// point at which the trail can name the person behind it.
	s.auditOrg(ctx, org.ID, caller, db.AuditActionSvcKeyMint,
		db.AuditTargetService, svc.DisplayName(), nil,
		fieldPayload(map[string]string{"prefix": key.KeyPrefix, "state": "active"}))

	writeJSON(w, http.StatusCreated, map[string]string{"key": plaintext})
}

// handleGUISetMemberRole: POST /orgs/{id}/members/{uid}/role
//
// Owner-only, deliberately. Admin sits below owner precisely so that
// widening the org gates in Phase 1 doesn't let an admin promote
// themselves — that would make the owner tier decorative.
func (s *Server) handleGUISetMemberRole(w http.ResponseWriter, r *http.Request) {
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
	switch role {
	case OrgRoleNone:
		http.NotFound(w, r)
		return
	case OrgRoleOwner:
		// fall through
	default:
		http.Error(w, "owner only", http.StatusForbidden)
		return
	}

	target, err := uuid.Parse(r.PathValue("uid"))
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	want := r.FormValue("role")
	wasRole, _ := s.RoleInOrg(r.Context(), target, org.ID)

	if err := db.SetMemberRole(r.Context(), s.Pool, org.ID, target, want); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Read the tier back rather than trusting the form value: RoleInOrg
	// treats accounts.owner_user_id as the authority and downgrades a
	// stale 'owner' members row to admin, so `want` is what was asked
	// for and this is what the org will actually enforce. A re-POST of
	// the tier someone already holds changes nothing and gets no row.
	nowRole, _ := s.RoleInOrg(r.Context(), target, org.ID)
	if nowRole != wasRole {
		s.auditOrg(r.Context(), org.ID, AuthedCaller{UserID: uid},
			db.AuditActionMemberRole, db.AuditTargetUser,
			auditUsername(r.Context(), s.Pool, target),
			rolePayload(wasRole), rolePayload(nowRole))
	}
	browserSubmit(w, r)
}
