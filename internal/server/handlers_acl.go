package server

// ACL surface — ACL Phase 2.
//
// Three views, because three audiences ask different questions:
//
//	GET  /api/v1/acl?pipe=<path>        who can touch this pipe?
//	GET  /api/v1/acl?principal=<name>   what can this principal reach?
//	GET  /api/v1/acl/whoami?pipe=<path> what can I do here, and why not?
//	POST /api/v1/acl/grant | /revoke
//
// Every view reports EFFECTIVE access with provenance, never the raw
// grant table. With defaults derived from (collar, pipe name) rather
// than stored, most access has no row behind it — a view built on
// acl_grants renders an almost-empty table and reports that nobody can
// reach alice.inbox when in fact every member can write to it.

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/acl"
	"github.com/pipescloud/ppz/internal/db"
)

type aclGrantRequest struct {
	Pipe      string `json:"pipe"`
	Principal string `json:"principal"`
	Perm      string `json:"perm"`
}

// resolveSubject turns a pipe path into the evaluator's Subject.
//
// The collar/manifold ambiguity is resolved by DB row, exactly as it is
// at pipe-create time: if the first segment names a source in this
// account, the path is collared and that source's creator owns it.
// Otherwise it is uncollared shared space.
func (s *Server) resolveSubject(r *http.Request, accountID uuid.UUID, path string) (acl.Subject, error) {
	if path == "" {
		return acl.Subject{}, errors.New("pipe is required")
	}
	parts := strings.Split(path, ".")
	subj := acl.Subject{Path: path, Name: parts[len(parts)-1]}
	if len(parts) < 2 {
		return subj, nil
	}
	src, err := db.GetSourceByHandle(r.Context(), s.Pool, accountID, parts[0])
	if err != nil {
		// No such source — uncollared (shared org space).
		return subj, nil
	}
	subj.Collar = src.Handle
	subj.Owner = src.CreatedByUserID
	return subj, nil
}

// callerPrincipal builds the acl.Principal for the authenticated caller.
func (s *Server) callerPrincipal(r *http.Request, accountID uuid.UUID) (acl.Principal, error) {
	caller := CallerFromCtx(r.Context())
	id := caller.Principal()
	if id == uuid.Nil {
		return acl.Principal{}, errors.New("no principal")
	}
	u, err := db.GetUser(r.Context(), s.Pool, id)
	if err != nil {
		return acl.Principal{}, err
	}
	role, err := s.RoleInOrg(r.Context(), id, accountID)
	if err != nil {
		return acl.Principal{}, err
	}
	return acl.Principal{ID: id, Name: u.DisplayName(), OrgRole: acl.OrgRole(role)}, nil
}

// orgPrincipals lists everyone who could hold access in the account:
// members (human and service) plus @everyone.
func (s *Server) orgPrincipals(r *http.Request, accountID uuid.UUID) ([]acl.Principal, error) {
	members, err := db.ListMembers(r.Context(), s.Pool, accountID)
	if err != nil {
		return nil, err
	}
	out := make([]acl.Principal, 0, len(members)+1)
	for _, m := range members {
		if m.ID == acl.EveryoneID {
			continue
		}
		role, err := s.RoleInOrg(r.Context(), m.ID, accountID)
		if err != nil {
			return nil, err
		}
		out = append(out, acl.Principal{ID: m.ID, Name: m.DisplayName(), OrgRole: acl.OrgRole(role)})
	}
	// The org owner may not appear in account_members on older rows.
	org, err := db.GetAccount(r.Context(), s.Pool, accountID)
	if err == nil && org.OwnerUserID != uuid.Nil {
		found := false
		for _, p := range out {
			if p.ID == org.OwnerUserID {
				found = true
				break
			}
		}
		if !found {
			if u, err := db.GetUser(r.Context(), s.Pool, org.OwnerUserID); err == nil {
				out = append(out, acl.Principal{ID: u.ID, Name: u.DisplayName(), OrgRole: acl.OrgRole(OrgRoleOwner)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// handleAPIACL serves both the by-pipe roster and the by-principal view.
func (s *Server) handleAPIACL(w http.ResponseWriter, r *http.Request) {
	org, role, err := s.resolveCallerOrg(r)
	if err != nil || role == OrgRoleNone {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	if principal := r.URL.Query().Get("principal"); principal != "" {
		s.serveByPrincipal(w, r, org, principal)
		return
	}
	s.serveRoster(w, r, org, r.URL.Query().Get("pipe"))
}

func (s *Server) serveRoster(w http.ResponseWriter, r *http.Request, org db.Account, pipe string) {
	subj, err := s.resolveSubject(r, org.ID, pipe)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	grants, err := db.ListACLGrants(r.Context(), s.Pool, org.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	all := make([]acl.Grant, 0, len(grants))
	for _, g := range grants {
		all = append(all, g.ToACL())
	}

	// The roster is visible to any principal holding ANY access —
	// including write-only. An inbox sender holds no read, and a naive
	// "can you read it" gate would hide the roster from every sender in
	// the org, defeating the point: a denied agent needs to know who to
	// ask.
	me, err := s.callerPrincipal(r, org.ID)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "no principal"})
		return
	}
	if !acl.CanSeeRoster(acl.Evaluate(me, subj, all)) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "E_PIPE_FORBIDDEN: no access to " + pipe,
		})
		return
	}

	principals, err := s.orgPrincipals(r, org.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	rows := make([]acl.RosterRow, 0, len(principals))
	for _, p := range principals {
		d := acl.Evaluate(p, subj, all)
		if d.Perm == 0 {
			continue
		}
		rows = append(rows, acl.RosterRow{Principal: p.Name, Decision: d})
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) serveByPrincipal(w http.ResponseWriter, r *http.Request, org db.Account, name string) {
	target, err := db.ResolvePrincipal(r.Context(), s.Pool, org.ID, org.Name, name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such principal: " + name})
		return
	}
	role, err := s.RoleInOrg(r.Context(), target.ID, org.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	p := acl.Principal{ID: target.ID, Name: target.DisplayName(), OrgRole: acl.OrgRole(role)}

	grants, err := db.ListACLGrantsForPrincipal(r.Context(), s.Pool, org.ID, target.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	applicable := make([]acl.Grant, 0, len(grants))
	for _, g := range grants {
		applicable = append(applicable, g.ToACL())
	}

	pipes, err := s.aclPipePaths(r, org.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	rows := make([]acl.PrincipalRow, 0, len(pipes))
	for _, path := range pipes {
		subj, err := s.resolveSubject(r, org.ID, path)
		if err != nil {
			continue
		}
		d := acl.Evaluate(p, subj, applicable)
		if d.Perm == 0 {
			continue
		}
		rows = append(rows, acl.PrincipalRow{Pipe: path, Decision: d})
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleAPIACLWhoami answers "what can I do here, and why not" — the
// question this feature generates most often.
func (s *Server) handleAPIACLWhoami(w http.ResponseWriter, r *http.Request) {
	org, role, err := s.resolveCallerOrg(r)
	if err != nil || role == OrgRoleNone {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	pipe := r.URL.Query().Get("pipe")
	subj, err := s.resolveSubject(r, org.ID, pipe)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	me, err := s.callerPrincipal(r, org.ID)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "no principal"})
		return
	}
	grants, err := db.ListACLGrantsForPrincipal(r.Context(), s.Pool, org.ID, me.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	applicable := make([]acl.Grant, 0, len(grants))
	for _, g := range grants {
		applicable = append(applicable, g.ToACL())
	}
	d := acl.Evaluate(me, subj, applicable)

	view := acl.WhoamiView{Pipe: pipe, Principal: me.Name, Decision: d}
	if rem := s.remediationFor(r, org, subj, me, d); rem != nil {
		view.Remediation = rem
	}
	writeJSON(w, http.StatusOK, view)
}

// remediationFor names the missing capability, the exact command that
// grants it, and who is able to run it — so a denied agent can ask the
// right principal over that principal's inbox instead of failing
// opaquely. Nil when nothing is missing.
func (s *Server) remediationFor(r *http.Request, org db.Account, subj acl.Subject, me acl.Principal, d acl.Decision) *acl.Remediation {
	var missing string
	switch {
	case !d.Has(acl.Read):
		missing = "read"
	case !d.Has(acl.Write):
		missing = "write"
	default:
		return nil
	}
	// Deduped by principal: the handle owner is very often also the org
	// owner, and listing the same person twice under two labels is noise
	// in the one place an agent is trying to work out who to ask.
	var by []string
	seen := map[uuid.UUID]bool{}
	addRunner := func(id uuid.UUID, label string) {
		if id == uuid.Nil || seen[id] {
			return
		}
		if u, err := db.GetUser(r.Context(), s.Pool, id); err == nil {
			seen[id] = true
			by = append(by, u.DisplayName()+" ("+label+")")
		}
	}
	if subj.Collar != "" {
		addRunner(subj.Owner, "handle owner")
	}
	addRunner(org.OwnerUserID, "org owner")
	return &acl.Remediation{
		Command:    "ppz pipe acl grant " + subj.Path + " " + me.Name + " " + missing,
		RunnableBy: by,
	}
}

// handleAPIACLGrant / handleAPIACLRevoke mutate the store. Both require
// admin on the target — a principal holding write cannot widen access.
func (s *Server) handleAPIACLGrant(w http.ResponseWriter, r *http.Request) {
	s.mutateACL(w, r, true)
}

func (s *Server) handleAPIACLRevoke(w http.ResponseWriter, r *http.Request) {
	s.mutateACL(w, r, false)
}

func (s *Server) mutateACL(w http.ResponseWriter, r *http.Request, grant bool) {
	var req aclGrantRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	org, role, err := s.resolveCallerOrg(r)
	if err != nil || role == OrgRoleNone {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	subj, err := s.resolveSubject(r, org.ID, req.Pipe)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	me, err := s.callerPrincipal(r, org.ID)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "no principal"})
		return
	}
	stored, err := db.ListACLGrantsForPrincipal(r.Context(), s.Pool, org.ID, me.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	mine := make([]acl.Grant, 0, len(stored))
	for _, g := range stored {
		mine = append(mine, g.ToACL())
	}
	if !acl.Evaluate(me, subj, mine).Has(acl.Admin) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "E_PIPE_FORBIDDEN: admin required on " + req.Pipe,
		})
		return
	}

	target, err := db.ResolvePrincipal(r.Context(), s.Pool, org.ID, org.Name, req.Principal)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such principal: " + req.Principal})
		return
	}

	if grant {
		if err := db.InsertACLGrant(r.Context(), s.Pool, org.ID, target.ID, req.Pipe, req.Perm, me.ID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	} else {
		perm := req.Perm
		if perm == "all" {
			perm = ""
		}
		if err := db.DeleteACLGrant(r.Context(), s.Pool, org.ID, target.ID, req.Pipe, perm); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"pipe": req.Pipe, "principal": target.DisplayName(), "perm": req.Perm,
	})
}

// aclPipePaths enumerates every pipe path in the account — the
// auto-provisioned pipes of each source plus user-created and
// uncollared rows.
func (s *Server) aclPipePaths(r *http.Request, accountID uuid.UUID) ([]string, error) {
	sources, err := db.ListSourcesForOrg(r.Context(), s.Pool, accountID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, src := range sources {
		prefix := src.Handle + "."
		if src.Manifold != "" {
			prefix = src.Manifold + "." + prefix
		}
		for _, p := range src.Pipes() {
			add(prefix + p)
		}
	}
	// User-created pipes hang off their source; uncollared ones live at
	// the account root.
	for _, src := range sources {
		rows, err := db.ListPipesForSource(r.Context(), s.Pool, src.ID)
		if err != nil {
			return nil, err
		}
		prefix := src.Handle + "."
		if src.Manifold != "" {
			prefix = src.Manifold + "." + prefix
		}
		for _, row := range rows {
			add(prefix + row.Name)
		}
	}
	uncollared, err := db.ListUncollaredPipesForAccount(r.Context(), s.Pool, accountID)
	if err != nil {
		return nil, err
	}
	for _, row := range uncollared {
		if row.Manifold != "" {
			add(row.Manifold + "." + row.Name)
			continue
		}
		add(row.Name)
	}
	sort.Strings(out)
	return out, nil
}
