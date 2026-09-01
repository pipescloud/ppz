package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/db"
)

// invite handlers — Phase 4 (multi-org + invitations).
//
// API surface (bearer-auth, OAuth-only — API keys are org-scoped and
// have no user identity):
//
//   POST /api/v1/orgs/{slug}/invites              owner-only
//   GET  /api/v1/orgs/{slug}/invites              owner-only
//   POST /api/v1/orgs/{slug}/invites/{id}/revoke  owner-only
//   GET  /api/v1/invites                          caller's pending invites
//   POST /api/v1/invites/{id}/accept              caller must match invitee_username
//   POST /api/v1/invites/{id}/decline             caller must match invitee_username
//
// GUI surface (session-auth, form-post):
//
//   POST /orgs/{id}/invites                       owner-only
//   POST /orgs/{id}/invites/{iid}/revoke          owner-only
//   POST /invites/{id}/accept
//   POST /invites/{id}/decline

// ─── API: invites ────────────────────────────────────────────────────────────

// handleAPICreateInvite: POST /api/v1/orgs/{slug}/invites
func (s *Server) handleAPICreateInvite(w http.ResponseWriter, r *http.Request) {
	caller := CallerFromCtx(r.Context())
	if caller.UserID == uuid.Nil {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "authentication required",
		})
		return
	}

	var req cliproto.CreateInviteRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username is required"})
		return
	}

	ctx, cancel := withTimeout(r)
	defer cancel()

	org, err := resolveOrg(ctx, s.Pool, r.PathValue("slug"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	if org.OwnerUserID != caller.UserID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only the org owner can invite"})
		return
	}

	callerUser, err := db.GetUser(ctx, s.Pool, caller.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if callerUser.Username == username {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot invite yourself"})
		return
	}

	inv, err := db.CreateInvite(ctx, s.Pool, org.ID, username, caller.UserID)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrAlreadyMember):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.Is(err, db.ErrDuplicatePendingInvite):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	s.auditOrg(ctx, org.ID, caller, db.AuditActionInviteCreate, db.AuditTargetInvite,
		inv.InviteeUsername, nil, invitePayload(inv.Status))

	writeJSON(w, http.StatusCreated, cliproto.CreateInviteReply{Invite: inviteToWire(inv, org.Name)})
}

// handleAPIListInvitesForOrg: GET /api/v1/orgs/{slug}/invites
func (s *Server) handleAPIListInvitesForOrg(w http.ResponseWriter, r *http.Request) {
	caller := CallerFromCtx(r.Context())
	if caller.UserID == uuid.Nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authentication required"})
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("slug"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	if org.OwnerUserID != caller.UserID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner only"})
		return
	}
	invites, err := db.ListInvitesForOrg(ctx, s.Pool, org.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]cliproto.Invite, 0, len(invites))
	for _, i := range invites {
		out = append(out, inviteToWire(i, org.Name))
	}
	writeJSON(w, http.StatusOK, cliproto.ListInvitesReply{Invites: out})
}

// handleAPIRevokeInvite: POST /api/v1/orgs/{slug}/invites/{id}/revoke
func (s *Server) handleAPIRevokeInvite(w http.ResponseWriter, r *http.Request) {
	caller := CallerFromCtx(r.Context())
	if caller.UserID == uuid.Nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authentication required"})
		return
	}
	inviteID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid invite id"})
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("slug"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	if org.OwnerUserID != caller.UserID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner only"})
		return
	}
	inv, err := db.GetInvite(ctx, s.Pool, inviteID)
	if err != nil || inv.AccountID != org.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "invite not found"})
		return
	}
	if err := db.RevokeInvite(ctx, s.Pool, inviteID); err != nil {
		if errors.Is(err, db.ErrInviteNotPending) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	s.auditOrg(ctx, org.ID, caller, db.AuditActionInviteRevoke, db.AuditTargetInvite,
		inv.InviteeUsername, invitePayload(db.InviteStatusPending),
		invitePayload(db.InviteStatusRevoked))

	w.WriteHeader(http.StatusNoContent)
}

// handleAPIListMyInvites: GET /api/v1/invites
func (s *Server) handleAPIListMyInvites(w http.ResponseWriter, r *http.Request) {
	caller := CallerFromCtx(r.Context())
	if caller.UserID == uuid.Nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authentication required"})
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	user, err := db.GetUser(ctx, s.Pool, caller.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	rows, err := db.ListPendingInvitesForUsername(ctx, s.Pool, user.Username)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]cliproto.Invite, 0, len(rows))
	for _, iw := range rows {
		out = append(out, inviteToWire(iw.Invite, iw.AccountName))
	}
	writeJSON(w, http.StatusOK, cliproto.ListInvitesReply{Invites: out})
}

// handleAPIAcceptInvite: POST /api/v1/invites/{id}/accept
func (s *Server) handleAPIAcceptInvite(w http.ResponseWriter, r *http.Request) {
	s.acceptOrDecline(w, r, true)
}

// handleAPIDeclineInvite: POST /api/v1/invites/{id}/decline
func (s *Server) handleAPIDeclineInvite(w http.ResponseWriter, r *http.Request) {
	s.acceptOrDecline(w, r, false)
}

func (s *Server) acceptOrDecline(w http.ResponseWriter, r *http.Request, accept bool) {
	caller := CallerFromCtx(r.Context())
	if caller.UserID == uuid.Nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authentication required"})
		return
	}
	inviteID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid invite id"})
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()

	// Read the invite before deciding it: the audit row is filed against
	// the invite's ORG and names the invitee, and neither is derivable
	// from an invite id once the status has moved on. A miss here just
	// means the decision below reports the not-found.
	inv, invErr := db.GetInvite(ctx, s.Pool, inviteID)

	if accept {
		err = db.AcceptInvite(ctx, s.Pool, inviteID, caller.UserID)
	} else {
		err = db.DeclineInvite(ctx, s.Pool, inviteID, caller.UserID)
	}
	if err != nil {
		switch {
		case errors.Is(err, db.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "invite not found"})
		case errors.Is(err, db.ErrInviteNotPending):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case err.Error() == "forbidden":
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "this invite is not for you"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}

	if invErr == nil {
		s.auditInviteDecision(ctx, inv, caller.UserID, accept)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── GUI: invites (form posts) ───────────────────────────────────────────────

// handleGUICreateInvite: POST /orgs/{id}/invites (form: username)
func (s *Server) handleGUICreateInvite(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	if username == "" {
		http.Error(w, "username required", http.StatusBadRequest)
		return
	}
	uid := UserIDFromCtx(r.Context())
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("id"))
	if err != nil {
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	if org.OwnerUserID != uid {
		http.Error(w, "owner only", http.StatusForbidden)
		return
	}
	caller, err := db.GetUser(ctx, s.Pool, uid)
	if err == nil && caller.Username == username {
		http.Error(w, "cannot invite yourself", http.StatusBadRequest)
		return
	}
	inv, err := db.CreateInvite(ctx, s.Pool, org.ID, username, uid)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrAlreadyMember), errors.Is(err, db.ErrDuplicatePendingInvite):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	s.auditOrg(ctx, org.ID, AuthedCaller{UserID: uid}, db.AuditActionInviteCreate,
		db.AuditTargetInvite, inv.InviteeUsername, nil, invitePayload(inv.Status))
	browserSubmit(w, r)
}

// handleGUIRevokeInvite: POST /orgs/{id}/invites/{iid}/revoke
func (s *Server) handleGUIRevokeInvite(w http.ResponseWriter, r *http.Request) {
	uid := UserIDFromCtx(r.Context())
	inviteID, err := uuid.Parse(r.PathValue("iid"))
	if err != nil {
		http.Error(w, "invalid invite id", http.StatusBadRequest)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, err := resolveOrg(ctx, s.Pool, r.PathValue("id"))
	if err != nil {
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	if org.OwnerUserID != uid {
		http.Error(w, "owner only", http.StatusForbidden)
		return
	}
	inv, err := db.GetInvite(ctx, s.Pool, inviteID)
	if err != nil || inv.AccountID != org.ID {
		http.Error(w, "invite not found", http.StatusNotFound)
		return
	}
	if err := db.RevokeInvite(ctx, s.Pool, inviteID); err != nil {
		if errors.Is(err, db.ErrInviteNotPending) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.auditOrg(ctx, org.ID, AuthedCaller{UserID: uid}, db.AuditActionInviteRevoke,
		db.AuditTargetInvite, inv.InviteeUsername,
		invitePayload(db.InviteStatusPending), invitePayload(db.InviteStatusRevoked))
	browserSubmit(w, r)
}

// handleGUIAcceptInvite: POST /invites/{id}/accept
func (s *Server) handleGUIAcceptInvite(w http.ResponseWriter, r *http.Request) {
	s.guiAcceptOrDecline(w, r, true)
}

// handleGUIDeclineInvite: POST /invites/{id}/decline
func (s *Server) handleGUIDeclineInvite(w http.ResponseWriter, r *http.Request) {
	s.guiAcceptOrDecline(w, r, false)
}

func (s *Server) guiAcceptOrDecline(w http.ResponseWriter, r *http.Request, accept bool) {
	uid := UserIDFromCtx(r.Context())
	inviteID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid invite id", http.StatusBadRequest)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()

	// Snapshotted before the decision, for the same reason as the API
	// path above: the org and invitee aren't recoverable from the id
	// once the status moves.
	inv, invErr := db.GetInvite(ctx, s.Pool, inviteID)

	if accept {
		err = db.AcceptInvite(ctx, s.Pool, inviteID, uid)
	} else {
		err = db.DeclineInvite(ctx, s.Pool, inviteID, uid)
	}
	if err != nil {
		switch {
		case errors.Is(err, db.ErrNotFound):
			http.Error(w, "invite not found", http.StatusNotFound)
		case errors.Is(err, db.ErrInviteNotPending):
			http.Error(w, err.Error(), http.StatusConflict)
		case err.Error() == "forbidden":
			http.Error(w, "this invite is not for you", http.StatusForbidden)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	if invErr == nil {
		s.auditInviteDecision(ctx, inv, uid, accept)
	}
	browserSubmit(w, r)
}

// ─── helpers ────────────────────────────────────────────────────────────────

func inviteToWire(inv db.Invite, accountName string) cliproto.Invite {
	out := cliproto.Invite{
		ID:               inv.ID.String(),
		AccountID:   inv.AccountID.String(),
		AccountName: accountName,
		InviteeUsername:  inv.InviteeUsername,
		InviterUserID:    inv.InviterUserID.String(),
		Status:           string(inv.Status),
		CreatedAt:        inv.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if inv.DecidedAt != nil {
		out.DecidedAt = inv.DecidedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return out
}
