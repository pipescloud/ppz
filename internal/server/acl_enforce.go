package server

// Enforcement wiring — ACL Phase 3.
//
// The per-org switch, the credential compilation that hangs off it, and
// the preview that makes turning it on a deliberate act.

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/acl"
	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// aclPipeRefs enumerates every pipe in the account with the JetStream
// stream backing it. The compiler needs the FULL set, not just what the
// caller can reach: the excluded pipes are what make the deny-list
// representation possible.
func (s *Server) aclPipeRefs(ctx context.Context, accountID uuid.UUID) ([]acl.PipeRef, error) {
	sources, err := db.ListSourcesForOrg(ctx, s.Pool, accountID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []acl.PipeRef
	add := func(manifold, handle, name string) {
		path := name
		if handle != "" {
			path = handle + "." + name
		}
		if manifold != "" {
			path = manifold + "." + path
		}
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, acl.PipeRef{
			Path: path,
			// The wire subject, not the logical path: BuildSubject
			// routes `heartbeat` to the presence family, and compiling
			// permissions from the path would deny every beat.
			Subject: natsubj.BuildSubject(accountID, manifold, handle, name),
			Stream:  natsubj.BuildStreamName(accountID, manifold, handle, name),
		})
	}
	for _, src := range sources {
		for _, name := range src.Pipes() {
			add(src.Manifold, src.Handle, name)
		}
		rows, err := db.ListPipesForSource(ctx, s.Pool, src.ID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			add(src.Manifold, src.Handle, row.Name)
		}
	}
	uncollared, err := db.ListUncollaredPipesForAccount(ctx, s.Pool, accountID)
	if err != nil {
		return nil, err
	}
	for _, row := range uncollared {
		add(row.Manifold, "", row.Name)
	}
	return out, nil
}

// subjectForRef rebuilds the evaluator's Subject for one pipe, resolving
// the collar the same way pipe creation does: by DB row.
func subjectFor(ref acl.PipeRef, sources map[string]db.Source) acl.Subject {
	subj := acl.Subject{Path: ref.Path}
	parts := splitPath(ref.Path)
	subj.Name = parts[len(parts)-1]
	if len(parts) < 2 {
		return subj
	}
	if src, ok := sources[parts[0]]; ok {
		subj.Collar = src.Handle
		subj.Owner = src.CreatedByUserID
	}
	return subj
}

func splitPath(p string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '.' {
			out = append(out, p[start:i])
			start = i + 1
		}
	}
	return append(out, p[start:])
}

// principalAccess evaluates one principal against every pipe in the
// account — the input the credential compiler consumes.
func (s *Server) principalAccess(ctx context.Context, accountID, principalID uuid.UUID) ([]acl.Access, error) {
	u, err := db.GetUser(ctx, s.Pool, principalID)
	if err != nil {
		return nil, err
	}
	role, err := s.RoleInOrg(ctx, principalID, accountID)
	if err != nil {
		return nil, err
	}
	p := acl.Principal{ID: principalID, Name: u.DisplayName(), OrgRole: acl.OrgRole(role)}

	stored, err := db.ListACLGrantsForPrincipal(ctx, s.Pool, accountID, principalID)
	if err != nil {
		return nil, err
	}
	grants := make([]acl.Grant, 0, len(stored))
	for _, g := range stored {
		grants = append(grants, g.ToACL())
	}

	srcRows, err := db.ListSourcesForOrg(ctx, s.Pool, accountID)
	if err != nil {
		return nil, err
	}
	sources := make(map[string]db.Source, len(srcRows))
	for _, src := range srcRows {
		sources[src.Handle] = src
	}

	refs, err := s.aclPipeRefs(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := make([]acl.Access, 0, len(refs))
	for _, ref := range refs {
		d := acl.Evaluate(p, subjectFor(ref, sources), grants)
		out = append(out, acl.Access{Pipe: ref, Perm: d.Perm})
	}
	return out, nil
}

// natsPermissionsFor is what /auth/exchange asks for. When the org has
// not opted in this returns the wide-open credential unchanged, without
// touching the ACL tables at all.
// natsPermissionsFor takes the org's enforcement state rather than
// re-reading it: the caller has already looked it up to put on the
// exchange reply, and two reads could in principle disagree if the
// switch were flipped between them.
func (s *Server) natsPermissionsFor(ctx context.Context, accountID, principalID uuid.UUID, enforced bool) (acl.Permissions, error) {
	if !enforced {
		return credentialPermissions(false, accountID.String(), nil), nil
	}
	access, err := s.principalAccess(ctx, accountID, principalID)
	if err != nil {
		return acl.Permissions{}, err
	}
	return credentialPermissions(true, accountID.String(), access), nil
}

// ─── Routes ──────────────────────────────────────────────────────────

// handleAPIACLEnforce reports or sets the org's enforcement state.
func (s *Server) handleAPIACLEnforce(w http.ResponseWriter, r *http.Request) {
	org, role, err := s.resolveCallerOrg(r)
	if err != nil || role == OrgRoleNone {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()

	if r.Method == http.MethodPost {
		if !role.CanAdministerOrg() {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "org admin only"})
			return
		}
		var req struct {
			Enforced bool `json:"enforced"`
		}
		if err := readJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		was, _ := db.ACLEnforced(ctx, s.Pool, org.ID)
		if err := db.SetACLEnforced(ctx, s.Pool, org.ID, req.Enforced); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.auditACL(ctx, org.ID, CallerFromCtx(r.Context()), db.AuditActionACLEnforce,
			db.AuditTargetOrg, org.Name, aclEnforceDelta(was), aclEnforceDelta(req.Enforced))
		s.notifyACLChanged(ctx, org.ID)
		writeJSON(w, http.StatusOK, map[string]bool{"enforced": req.Enforced})
		return
	}

	on, err := db.ACLEnforced(ctx, s.Pool, org.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enforced": on})
}

type previewWire struct {
	Enforced            bool                `json:"enforced"`
	PlaceholderOwnedOrg bool                `json:"placeholder_owned_org"`
	OrphanedHandles     []map[string]string `json:"orphaned_handles"`
	SharedTerminals     []map[string]string `json:"shared_terminals"`
	InboxReadLoss       []map[string]string `json:"inbox_read_loss"`
	CollaredUserPipes   []map[string]string `json:"collared_user_pipes"`
	Empty               bool                `json:"empty"`
}

// handleAPIACLPreview reports what enabling enforcement would take away.
func (s *Server) handleAPIACLPreview(w http.ResponseWriter, r *http.Request) {
	org, role, err := s.resolveCallerOrg(r)
	if err != nil || role == OrgRoleNone {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()

	in, err := s.previewInput(ctx, org)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	p := acl.BuildPreview(in)
	enforced, _ := db.ACLEnforced(ctx, s.Pool, org.ID)

	pairs := func(k1, k2 string, rows [][2]string) []map[string]string {
		out := make([]map[string]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]string{k1: r[0], k2: r[1]})
		}
		return out
	}
	orph := make([][2]string, 0, len(p.OrphanedHandles))
	for _, o := range p.OrphanedHandles {
		orph = append(orph, [2]string{o.Handle, o.Owner})
	}
	term := make([][2]string, 0, len(p.SharedTerminals))
	for _, t := range p.SharedTerminals {
		term = append(term, [2]string{t.Handle, t.Owner})
	}
	inbox := make([][2]string, 0, len(p.InboxReadLoss))
	for _, l := range p.InboxReadLoss {
		inbox = append(inbox, [2]string{l.Pipe, l.Owner})
	}
	user := make([][2]string, 0, len(p.CollaredUserPipes))
	for _, l := range p.CollaredUserPipes {
		user = append(user, [2]string{l.Pipe, l.Owner})
	}

	writeJSON(w, http.StatusOK, previewWire{
		Enforced:            enforced,
		PlaceholderOwnedOrg: p.PlaceholderOwnedOrg,
		OrphanedHandles:     pairs("handle", "owner", orph),
		SharedTerminals:     pairs("handle", "owner", term),
		InboxReadLoss:       pairs("pipe", "owner", inbox),
		CollaredUserPipes:   pairs("pipe", "owner", user),
		Empty:               p.IsEmpty(),
	})
}

// previewInput reads the org's shape out of the database for the pure
// preview builder.
func (s *Server) previewInput(ctx context.Context, org db.Account) (acl.PreviewInput, error) {
	members, err := db.ListMembers(ctx, s.Pool, org.ID)
	if err != nil {
		return acl.PreviewInput{}, err
	}
	in := acl.PreviewInput{OrgOwnerID: org.OwnerUserID}
	memberIDs := map[uuid.UUID]bool{}
	for _, m := range members {
		if m.ID == acl.EveryoneID {
			continue
		}
		memberIDs[m.ID] = true
		role, err := s.RoleInOrg(ctx, m.ID, org.ID)
		if err != nil {
			return acl.PreviewInput{}, err
		}
		in.Members = append(in.Members, acl.Principal{
			ID: m.ID, Name: m.DisplayName(), OrgRole: acl.OrgRole(role),
		})
	}
	if org.OwnerUserID != uuid.Nil {
		memberIDs[org.OwnerUserID] = true
		if owner, err := db.GetUser(ctx, s.Pool, org.OwnerUserID); err == nil {
			in.OrgOwnerIsPlaceholder = owner.Username == "unauthenticated"
			found := false
			for _, m := range in.Members {
				if m.ID == owner.ID {
					found = true
				}
			}
			if !found {
				in.Members = append(in.Members, acl.Principal{
					ID: owner.ID, Name: owner.DisplayName(), OrgRole: acl.OrgOwner,
				})
			}
		}
	}

	srcRows, err := db.ListSourcesForOrg(ctx, s.Pool, org.ID)
	if err != nil {
		return acl.PreviewInput{}, err
	}
	for _, src := range srcRows {
		in.Sources = append(in.Sources, acl.SourceRef{
			Handle:        src.Handle,
			Kind:          string(src.Kind),
			OwnerID:       src.CreatedByUserID,
			OwnerIsMember: memberIDs[src.CreatedByUserID],
		})
	}
	refs, err := s.aclPipeRefs(ctx, org.ID)
	if err != nil {
		return acl.PreviewInput{}, err
	}
	in.Pipes = refs
	return in, nil
}

// SecurityRight is one row of the Security tab's rights table.
type SecurityRight struct {
	Principal string
	Pipe      string
	Read      bool
	Write     bool
	Admin     bool
	Via       string
}

// securityRights builds the rights table: principals and what each can
// reach, computed live and labelled with provenance rather than
// materialised into grant rows.
//
// Materialising on enable was considered and rejected — pipes created
// afterwards would have no rows and so no access, forcing pipe create to
// materialise too, and the table would grow with principals × pipes.
// Deriving keeps new pipes correct for free, and "reset to defaults" is
// a delete.
func (s *Server) securityRights(ctx context.Context, org db.Account) []SecurityRight {
	members, err := db.ListMembers(ctx, s.Pool, org.ID)
	if err != nil {
		return nil
	}
	srcRows, err := db.ListSourcesForOrg(ctx, s.Pool, org.ID)
	if err != nil {
		return nil
	}
	sources := make(map[string]db.Source, len(srcRows))
	for _, src := range srcRows {
		sources[src.Handle] = src
	}
	refs, err := s.aclPipeRefs(ctx, org.ID)
	if err != nil {
		return nil
	}

	var out []SecurityRight
	for _, m := range members {
		if m.ID == acl.EveryoneID {
			continue
		}
		role, err := s.RoleInOrg(ctx, m.ID, org.ID)
		if err != nil {
			continue
		}
		p := acl.Principal{ID: m.ID, Name: m.DisplayName(), OrgRole: acl.OrgRole(role)}
		stored, err := db.ListACLGrantsForPrincipal(ctx, s.Pool, org.ID, m.ID)
		if err != nil {
			continue
		}
		grants := make([]acl.Grant, 0, len(stored))
		for _, g := range stored {
			grants = append(grants, g.ToACL())
		}
		for _, ref := range refs {
			d := acl.Evaluate(p, subjectFor(ref, sources), grants)
			if d.Perm == 0 {
				continue
			}
			out = append(out, SecurityRight{
				Principal: p.Name,
				Pipe:      ref.Path,
				Read:      d.Has(acl.Read),
				Write:     d.Has(acl.Write),
				Admin:     d.Has(acl.Admin),
				Via:       acl.ViaLabel(d),
			})
		}
	}
	return out
}

// handleGUISetACLEnforce is the Security tab's toggle. Org admin only,
// consistent with the other org-management surfaces.
func (s *Server) handleGUISetACLEnforce(w http.ResponseWriter, r *http.Request) {
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
	switch {
	case role == OrgRoleNone:
		http.NotFound(w, r)
		return
	case !role.CanAdministerOrg():
		http.Error(w, "org admin only", http.StatusForbidden)
		return
	}
	on := r.FormValue("enforced") == "on" || r.FormValue("enforced") == "true"
	was, _ := db.ACLEnforced(r.Context(), s.Pool, org.ID)
	if err := db.SetACLEnforced(r.Context(), s.Pool, org.ID, on); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Session path: no API key, so the trail names the person directly.
	s.auditACL(r.Context(), org.ID, AuthedCaller{UserID: uid}, db.AuditActionACLEnforce,
		db.AuditTargetOrg, org.Name, aclEnforceDelta(was), aclEnforceDelta(on))
	s.notifyACLChanged(r.Context(), org.ID)
	browserSubmit(w, r)
}

// notifyACLChanged tells every daemon in the account to re-fetch its
// credential.
//
// NATS evaluates permissions only at connect/credential load, so a
// grant, a revoke or the enforcement switch does not reach a live
// connection on its own — it would otherwise not land until the
// credential expired, leaving a principal using access they no longer
// have for up to a full refresh interval.
//
// Best-effort by design: the credential TTL is the backstop, so a failed
// publish delays the change rather than losing it. Callers do not fail
// the mutation over it.
func (s *Server) notifyACLChanged(ctx context.Context, accountID uuid.UUID) {
	oa, err := s.AccountPool.Get(ctx, accountID)
	if err != nil || oa == nil || oa.NC == nil {
		return
	}
	_ = oa.NC.Publish(natsubj.SystemACLSubject(accountID), nil)
	_ = oa.NC.Flush()
}

// requirePipeAdmin refuses a pipe-management operation unless the caller
// holds admin on that pipe.
//
// Only bites while the org has opted into enforcement — with it off this
// returns nil without touching the ACL tables, so nothing about an
// un-opted-in org's behaviour changes.
//
// Applies to retention changes (`ppz pipe set`) as well as create and
// destroy: retention is what `admin` is defined to cover, and pipe set
// deliberately reaches reserved auto-pipes that `pipe create` can never
// name — so leaving it ungated would let any member shorten the TTL on
// another principal's stdout and silently discard their history.
func (s *Server) requirePipeAdmin(ctx context.Context, key db.APIKey, path string) error {
	// No pool means no enforcement state to consult — the validation-only
	// handler tests construct a Server without one.
	if s == nil || s.Pool == nil {
		return nil
	}
	enforced, err := db.ACLEnforced(ctx, s.Pool, key.AccountID)
	if err != nil || !enforced {
		return nil
	}
	principal := key.Actor()
	u, err := db.GetUser(ctx, s.Pool, principal)
	if err != nil {
		return errors.New("no principal")
	}
	role, err := s.RoleInOrg(ctx, principal, key.AccountID)
	if err != nil {
		return err
	}
	p := acl.Principal{ID: principal, Name: u.DisplayName(), OrgRole: acl.OrgRole(role)}

	srcRows, err := db.ListSourcesForOrg(ctx, s.Pool, key.AccountID)
	if err != nil {
		return err
	}
	sources := make(map[string]db.Source, len(srcRows))
	for _, src := range srcRows {
		sources[src.Handle] = src
	}
	stored, err := db.ListACLGrantsForPrincipal(ctx, s.Pool, key.AccountID, principal)
	if err != nil {
		return err
	}
	grants := make([]acl.Grant, 0, len(stored))
	for _, g := range stored {
		grants = append(grants, g.ToACL())
	}
	if !acl.Evaluate(p, subjectFor(acl.PipeRef{Path: path}, sources), grants).Has(acl.Admin) {
		return errPipeForbidden
	}
	return nil
}

// errPipeForbidden is the sentinel the pipe handlers map to
// E_PIPE_FORBIDDEN.
var errPipeForbidden = errors.New("admin required on this pipe")
