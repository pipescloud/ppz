package server

// Pipe and source management from the org page's Pipes tab. Same
// operations as `ppz pipe create|destroy` / `ppz source create|destroy`,
// via the shared ops in pipe_ops.go, so the GUI can't drift from the
// CLI's validation and collision rules.
//
// Any org member may create and destroy (CLI parity). When the org has
// opted into ACL enforcement, destroy additionally needs admin on every
// pipe it removes — the same check `ppz pipe set` applies.
//
// Sources are addressed by full path (manifold.handle) and looked up
// exactly: handles are unique per manifold, not per org.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/db"
)

// sessionKey is the synthetic API key a session caller acts through. It
// has no id, so the audit trail records the change as "via web".
func sessionKey(org db.Account, r *http.Request) db.APIKey {
	uid := UserIDFromCtx(r.Context())
	return db.APIKey{AccountID: org.ID, CreatedByUserID: uid, PrincipalUserID: uid}
}

func writeOpErr(w http.ResponseWriter, e *cliproto.Error) {
	http.Error(w, e.Message, cliproto.HTTPStatus(e.Code))
}

// sourceAt resolves a "manifold.handle" path (or a bare handle at root).
func (s *Server) sourceAt(ctx context.Context, orgID uuid.UUID, path string) (db.Source, *cliproto.Error) {
	manifold, handle := splitManifoldName(path)
	if e := validateManifold(manifold); e != nil {
		return db.Source{}, e
	}
	src, err := db.GetSourceAt(ctx, s.Pool, orgID, manifold, handle)
	if errors.Is(err, db.ErrNotFound) {
		return db.Source{}, cliproto.NewSourceNotFound(path)
	}
	if err != nil {
		return db.Source{}, internalErr(err.Error())
	}
	return src, nil
}

// requireAdminOn applies the ACL admin check to each pipe path. A no-op
// unless the org enforces ACLs.
func (s *Server) requireAdminOn(ctx context.Context, key db.APIKey, paths ...string) *cliproto.Error {
	for _, p := range paths {
		if err := s.requirePipeAdmin(ctx, key, p); err != nil {
			if errors.Is(err, errPipeForbidden) {
				return &cliproto.Error{Code: cliproto.EPipeForbidden, Message: "admin required on " + p}
			}
			return internalErr(err.Error())
		}
	}
	return nil
}

// handleGUICreatePipe: POST /orgs/{id}/pipes
//
//	kind=source  namespace, name        → message source at namespace
//	kind=pipe    namespace, name        → uncollared pipe at namespace
//	kind=pipe    source=<path>, name    → user pipe on that source (the
//	                                      namespace is the source's)
func (s *Server) handleGUICreatePipe(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, ok := s.resolveMemberOrg(ctx, w, r)
	if !ok {
		return
	}
	key := sessionKey(org, r)
	namespace := strings.TrimSpace(r.FormValue("namespace"))
	name := strings.TrimSpace(r.FormValue("name"))
	source := strings.TrimSpace(r.FormValue("source"))

	var e *cliproto.Error
	switch r.FormValue("kind") {
	case "source":
		_, e = s.createSource(ctx, key, namespace, name, db.SourceKindMessage)
	case "pipe":
		if source == "" {
			_, e = s.createUncollaredPipe(ctx, key, namespace, name, nil, nil, nil)
			break
		}
		src, se := s.sourceAt(ctx, org.ID, source)
		if se != nil {
			e = se
			break
		}
		_, e = s.createCollaredPipe(ctx, key, src, name, nil, nil, nil)
	default:
		http.Error(w, "kind must be 'pipe' or 'source'", http.StatusBadRequest)
		return
	}
	if e != nil {
		writeOpErr(w, e)
		return
	}
	browserSubmit(w, r)
}

// handleGUIDeletePipe: POST /orgs/{id}/pipes/delete
//
//	source=<path>, name    → that source's user pipe
//	namespace, name        → uncollared pipe
//
// Auto-provisioned pipes are refused: removing one leaves the source
// looking alive but broken. Delete the source instead.
func (s *Server) handleGUIDeletePipe(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, ok := s.resolveMemberOrg(ctx, w, r)
	if !ok {
		return
	}
	key := sessionKey(org, r)
	name := strings.TrimSpace(r.FormValue("name"))
	source := strings.TrimSpace(r.FormValue("source"))

	if source == "" {
		namespace := strings.TrimSpace(r.FormValue("namespace"))
		path := name
		if namespace != "" {
			path = namespace + "." + name
		}
		if e := s.requireAdminOn(ctx, key, path); e != nil {
			writeOpErr(w, e)
			return
		}
		if e := s.destroyUncollaredPipe(ctx, key, namespace, name); e != nil {
			writeOpErr(w, e)
			return
		}
		browserSubmit(w, r)
		return
	}

	src, e := s.sourceAt(ctx, org.ID, source)
	if e != nil {
		writeOpErr(w, e)
		return
	}
	if src.IsAutoPipe(name) {
		http.Error(w, name+" is provisioned by source "+source+"; delete the source instead", http.StatusConflict)
		return
	}
	if _, err := db.GetPipeByName(ctx, s.Pool, src.ID, name); errors.Is(err, db.ErrNotFound) {
		writeOpErr(w, cliproto.NewPipeNotFound(name, src.Handle))
		return
	}
	if e := s.requireAdminOn(ctx, key, src.Handle+"."+name); e != nil {
		writeOpErr(w, e)
		return
	}
	if e := s.destroyCollaredPipe(ctx, key, src, name); e != nil {
		writeOpErr(w, e)
		return
	}
	browserSubmit(w, r)
}

// handleGUIDeleteSource: POST /orgs/{id}/sources/delete (source=<path>)
// removes the source and every pipe under it.
func (s *Server) handleGUIDeleteSource(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, ok := s.resolveMemberOrg(ctx, w, r)
	if !ok {
		return
	}
	key := sessionKey(org, r)
	src, e := s.sourceAt(ctx, org.ID, strings.TrimSpace(r.FormValue("source")))
	if e != nil {
		writeOpErr(w, e)
		return
	}
	userPipes, err := db.ListPipesForSource(ctx, s.Pool, src.ID)
	if err != nil {
		writeOpErr(w, internalErr(err.Error()))
		return
	}
	paths := make([]string, 0, len(userPipes)+len(src.Pipes()))
	for _, p := range src.Pipes() {
		paths = append(paths, src.Handle+"."+p)
	}
	for _, p := range userPipes {
		paths = append(paths, src.Handle+"."+p.Name)
	}
	if e := s.requireAdminOn(ctx, key, paths...); e != nil {
		writeOpErr(w, e)
		return
	}
	if e := s.destroySource(ctx, key, src); e != nil {
		writeOpErr(w, e)
		return
	}
	browserSubmit(w, r)
}

// handleGUIPurgePipe: POST /orgs/{id}/pipes/purge
//
//	source=<path>, name    → that source's pipe (auto-provisioned included)
//	namespace, name        → uncollared pipe
//
// Empties the pipe's buffer. Same gate as delete: any member, plus ACL
// admin on the pipe while the org enforces ACLs.
func (s *Server) handleGUIPurgePipe(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()
	org, ok := s.resolveMemberOrg(ctx, w, r)
	if !ok {
		return
	}
	key := sessionKey(org, r)
	name := strings.TrimSpace(r.FormValue("name"))
	source := strings.TrimSpace(r.FormValue("source"))

	if source == "" {
		namespace := strings.TrimSpace(r.FormValue("namespace"))
		path := name
		if namespace != "" {
			path = namespace + "." + name
		}
		if e := s.requireAdminOn(ctx, key, path); e != nil {
			writeOpErr(w, e)
			return
		}
		if _, e := s.purgeUncollaredPipe(ctx, key, namespace, name); e != nil {
			writeOpErr(w, e)
			return
		}
		browserSubmit(w, r)
		return
	}
	src, e := s.sourceAt(ctx, org.ID, source)
	if e != nil {
		writeOpErr(w, e)
		return
	}
	if e := s.requireAdminOn(ctx, key, src.Handle+"."+name); e != nil {
		writeOpErr(w, e)
		return
	}
	if _, e := s.purgeCollaredPipe(ctx, key, src, name); e != nil {
		writeOpErr(w, e)
		return
	}
	browserSubmit(w, r)
}
