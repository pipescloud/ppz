package server

// Pipe and source lifecycle, shared by the bearer API (the CLI, via the
// daemon) and the session GUI, so both apply the same validation,
// collision rules, stream provisioning, audit and ACL nudges. Callers own
// the transport: decoding the request, locating the source, rendering
// the result.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/natsubj"
)

func internalErr(msg string) *cliproto.Error {
	return &cliproto.Error{Code: "E_INTERNAL", Message: msg}
}

func validateManifold(manifold string) *cliproto.Error {
	if manifold == "" {
		return nil
	}
	for _, seg := range strings.Split(manifold, ".") {
		if err := natsubj.ValidateHandle(seg); err != nil {
			return &cliproto.Error{Code: cliproto.EInvalidManifold, Message: "manifold segment invalid: " + seg}
		}
	}
	return nil
}

// validateUserPipe keeps natsubj's two failure modes distinct ("invalid
// pipe name" vs "name is reserved") so the user sees which one tripped.
func validateUserPipe(name string) *cliproto.Error {
	if err := natsubj.ValidateUserPipeName(name); err != nil {
		if err.Error() == "name is reserved" {
			return cliproto.NewInvalidPipeReserved(name)
		}
		return cliproto.NewInvalidPipeName(name)
	}
	return nil
}

// pipeCreated is a new pipe plus the retention its stream was given.
type pipeCreated struct {
	pipe     db.Pipe
	maxAge   time.Duration
	maxMsgs  int
	maxBytes int64
}

func (s *Server) createSource(ctx context.Context, key db.APIKey, manifold, handle string, kind db.SourceKind) (db.Source, *cliproto.Error) {
	if err := natsubj.ValidateHandle(handle); err != nil {
		return db.Source{}, cliproto.NewInvalidHandle(handle)
	}
	if e := validateManifold(manifold); e != nil {
		return db.Source{}, e
	}
	if kind == "" {
		kind = db.SourceKindMessage
	}
	if kind != db.SourceKindMessage && kind != db.SourceKindPTY {
		return db.Source{}, &cliproto.Error{Code: "E_INVALID_KIND", Message: "kind must be 'message' or 'pty'"}
	}

	// Phase 1.5.1 first-wins collision rule (both directions):
	// (a) reject if an uncollared pipe with this name already exists at
	//     this manifold;
	// (b) reject if there's already at least one pipe (collared or
	//     uncollared) under the manifold-prefix path the new source's
	//     auto-pipes would occupy — i.e. manifold==manifold+"."+handle
	//     (or handle when manifold is empty).
	if taken, err := db.UncollaredPipeExists(ctx, s.Pool, key.AccountID, manifold, handle); err != nil {
		return db.Source{}, internalErr(err.Error())
	} else if taken {
		return db.Source{}, cliproto.NewNameTakenByUncollaredPipe(handle, manifold)
	}
	reservedPath := handle
	if manifold != "" {
		reservedPath = manifold + "." + handle
	}
	if anyPipe, err := db.PipesExistAtManifold(ctx, s.Pool, key.AccountID, reservedPath); err != nil {
		return db.Source{}, internalErr(err.Error())
	} else if anyPipe {
		return db.Source{}, cliproto.NewManifoldReservedBySource(reservedPath, handle, manifold)
	}

	src, err := db.InsertSource(ctx, s.Pool, key.AccountID, key.Actor(), manifold, handle, kind)
	if err != nil {
		if errors.Is(err, db.ErrHandleTaken) {
			return db.Source{}, cliproto.NewSourceTaken(handle)
		}
		return db.Source{}, internalErr(err.Error())
	}

	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		return db.Source{}, internalErr("account: " + err.Error())
	}
	// Phase 1.5.1: auto-pipes provisioned at the source's manifold
	// via the four-role builder. For root-manifold sources (the
	// pre-1.5.1 case) the stream name is `pipe_<orgshort>_<handle>_<pipe>`
	// — a wire-format change from the legacy `source_…` prefix that
	// requires a Reset Database cutover.
	if err := ensureSourceStreams(ctx, s.Pool, js, key.AccountID, src); err != nil {
		return db.Source{}, internalErr(err.Error())
	}

	s.auditSource(ctx, key, db.AuditActionSourceCreate, src, nil, sourceSnapshot(src, nil))
	// The account's pipe set just changed, and a compiled credential
	// enumerates it. Without this nudge the caller cannot use the pipe
	// it just created until its credential expires — the "created a
	// pipe, denied for five minutes" trap. No-op when the org has not
	// opted into enforcement.
	s.notifyACLChanged(ctx, key.AccountID)
	return src, nil
}

// createCollaredPipe adds a user pipe to src. It inherits the source's
// manifold.
func (s *Server) createCollaredPipe(ctx context.Context, key db.APIKey, src db.Source, name string, ttl, maxMsgs *int, maxBytes *int64) (pipeCreated, *cliproto.Error) {
	if e := validateUserPipe(name); e != nil {
		return pipeCreated{}, e
	}
	pipe, err := db.InsertPipe(ctx, s.Pool, key.AccountID, src.Manifold, &src.ID, key.Actor(), name, ttl, maxMsgs, maxBytes)
	if err != nil {
		if errors.Is(err, db.ErrPipeNameTaken) {
			return pipeCreated{}, cliproto.NewPipeTaken(name, src.Handle)
		}
		return pipeCreated{}, internalErr(err.Error())
	}
	return s.provisionPipe(ctx, key, pipe, src.Manifold, src.Handle)
}

// createUncollaredPipe adds a sourceless pipe at manifold.
func (s *Server) createUncollaredPipe(ctx context.Context, key db.APIKey, manifold, name string, ttl, maxMsgs *int, maxBytes *int64) (pipeCreated, *cliproto.Error) {
	if e := validateManifold(manifold); e != nil {
		return pipeCreated{}, e
	}
	if e := validateUserPipe(name); e != nil {
		return pipeCreated{}, e
	}

	// Phase 1.5.1 first-wins collision rule: reject if a source with the
	// same name already exists at this manifold.
	if exists, err := db.SourceExistsAtManifold(ctx, s.Pool, key.AccountID, manifold, name); err != nil {
		return pipeCreated{}, internalErr(err.Error())
	} else if exists {
		return pipeCreated{}, cliproto.NewNameTakenBySource(name, manifold)
	}
	// Phase 1.5.1 first-wins collision rule: also reject if the manifold
	// path itself collides with a reserved source-prefix. Source X at
	// manifold M reserves the prefix M.X via its auto-pipes — so any pipe
	// at manifold M.X (or M.X.<deeper>) would land on the source's
	// subjects. Walk the segments and check each prefix.
	if manifold != "" {
		segs := strings.Split(manifold, ".")
		for i, seg := range segs {
			parent := strings.Join(segs[:i], ".")
			reserved, err := db.SourceExistsAtManifold(ctx, s.Pool, key.AccountID, parent, seg)
			if err != nil {
				return pipeCreated{}, internalErr(err.Error())
			}
			if reserved {
				prefix := strings.Join(segs[:i+1], ".")
				return pipeCreated{}, cliproto.NewManifoldReservedBySource(prefix, seg, parent)
			}
		}
	}

	pipe, err := db.InsertPipe(ctx, s.Pool, key.AccountID, manifold, nil, key.Actor(), name, ttl, maxMsgs, maxBytes)
	if err != nil {
		if errors.Is(err, db.ErrPipeNameTaken) {
			return pipeCreated{}, cliproto.NewUncollaredPipeTaken(name, manifold)
		}
		return pipeCreated{}, internalErr(err.Error())
	}
	return s.provisionPipe(ctx, key, pipe, manifold, "")
}

// provisionPipe creates the stream for a freshly inserted pipe row, then
// audits and nudges ACL subscribers. handle is "" for uncollared pipes.
func (s *Server) provisionPipe(ctx context.Context, key db.APIKey, pipe db.Pipe, manifold, handle string) (pipeCreated, *cliproto.Error) {
	maxAge, maxMsgs, maxBytes := resolveRetention(pipeLayer(pipe))
	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		return pipeCreated{}, internalErr("account: " + err.Error())
	}
	if err := ensurePipeStreamWithRetention(ctx, js, key.AccountID, manifold, handle, pipe.Name, maxAge, maxMsgs, maxBytes); err != nil {
		return pipeCreated{}, internalErr(err.Error())
	}
	s.auditPipe(ctx, key, db.AuditActionPipeCreate,
		cliproto.FormatPipePath(manifold, handle, pipe.Name),
		nil, snapshotRetention(maxAge, maxMsgs, maxBytes).mustJSON())
	// See createSource: the compiled credential enumerates pipes.
	s.notifyACLChanged(ctx, key.AccountID)
	return pipeCreated{pipe: pipe, maxAge: maxAge, maxMsgs: maxMsgs, maxBytes: maxBytes}, nil
}

// destroyCollaredPipe removes one of src's pipes. Auto-provisioned pipes
// have no row and are destroyed by stream deletion alone.
func (s *Server) destroyCollaredPipe(ctx context.Context, key db.APIKey, src db.Source, name string) *cliproto.Error {
	// Snapshot the row before it goes, so the audit trail can state what
	// the pipe retained. Missing row = an auto-pipe with no override;
	// the zero Pipe resolves to the defaults, which is the truth.
	prev, _ := db.GetPipeByName(ctx, s.Pool, src.ID, name)

	if err := db.DeletePipe(ctx, s.Pool, src.ID, name); err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			return internalErr(err.Error())
		}
		// Auto-provisioned pipes (broadcast, inbox, etc.) are
		// JetStream-only — not in the pipes table. Allow destroying
		// them directly via stream deletion rather than returning
		// E_PIPE_NOT_FOUND for pipes the user can see in ppz ls.
		if !src.IsAutoPipe(name) {
			return cliproto.NewPipeNotFound(name, src.Handle)
		}
	}
	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		return internalErr("org account: " + err.Error())
	}
	if err := deletePipeStream(ctx, js, key.AccountID, src.Manifold, src.Handle, name); err != nil {
		return internalErr(err.Error())
	}

	// A destroy has no "after". `before` is the retention the pipe had —
	// resolved, so the row states what was actually lost rather than
	// which columns happened to be non-NULL.
	prevAge, prevMsgs, prevBytes := pipeRetention(prev)
	s.auditPipe(ctx, key, db.AuditActionPipeDestroy,
		cliproto.FormatPipePath(src.Manifold, src.Handle, name),
		snapshotRetention(prevAge, prevMsgs, prevBytes).mustJSON(), nil)
	// The account's pipe set just changed, and a compiled credential
	// enumerates it. No-op when the org has not opted into enforcement.
	s.notifyACLChanged(ctx, key.AccountID)
	return nil
}

func (s *Server) destroyUncollaredPipe(ctx context.Context, key db.APIKey, manifold, name string) *cliproto.Error {
	if name == "" {
		return cliproto.New(cliproto.EInvalidPipe)
	}
	if err := natsubj.ValidateUserPipeName(name); err != nil {
		return cliproto.NewInvalidPipeName(name)
	}
	if e := validateManifold(manifold); e != nil {
		return e
	}

	// Read the row before deleting it — the audit entry's `before` is the
	// retention that was lost, and after the DELETE there is nothing left
	// to read it from. Best-effort: a missing row here just means the
	// delete below reports the not-found.
	prev, _ := db.GetUncollaredPipeByName(ctx, s.Pool, key.AccountID, manifold, name)

	if err := db.DeleteUncollaredPipe(ctx, s.Pool, key.AccountID, manifold, name); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return cliproto.NewUncollaredPipeNotFound(name, manifold)
		}
		return internalErr(err.Error())
	}
	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		return internalErr("account: " + err.Error())
	}
	if err := deleteUncollaredPipeStream(ctx, js, key.AccountID, manifold, name); err != nil {
		return internalErr(err.Error())
	}

	prevAge, prevMsgs, prevBytes := pipeRetention(prev)
	s.auditPipe(ctx, key, db.AuditActionPipeDestroy,
		cliproto.FormatPipePath(manifold, "", name),
		snapshotRetention(prevAge, prevMsgs, prevBytes).mustJSON(), nil)
	s.notifyACLChanged(ctx, key.AccountID)
	return nil
}

// destroySource removes src and every pipe under it: user pipe rows go
// with it via CASCADE, streams are deleted best-effort. Deletes by id, so
// a same-named source in another manifold is untouched.
func (s *Server) destroySource(ctx context.Context, key db.APIKey, src db.Source) *cliproto.Error {
	// Snapshot user-created pipe names before CASCADE removes them.
	userPipes, err := db.ListPipesForSource(ctx, s.Pool, src.ID)
	if err != nil {
		return internalErr(err.Error())
	}
	if err := db.DeleteSourceByID(ctx, s.Pool, key.AccountID, src.ID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return cliproto.NewSourceNotFound(src.Handle)
		}
		return internalErr(err.Error())
	}

	// Stream cleanup is best-effort — the DB row is already gone so orphaned
	// streams are storage waste only, not a correctness problem.
	if js, err := s.JSFor(ctx, key.AccountID); err == nil {
		for _, p := range userPipes {
			_ = deletePipeStream(ctx, js, key.AccountID, p.Manifold, src.Handle, p.Name)
		}
		for _, p := range src.Pipes() {
			_ = deletePipeStream(ctx, js, key.AccountID, src.Manifold, src.Handle, p)
		}
	}

	s.auditSource(ctx, key, db.AuditActionSourceDestroy, src, sourceSnapshot(src, userPipes), nil)
	// The account's pipe set just changed, and a compiled credential
	// enumerates it. No-op when the org has not opted into enforcement.
	s.notifyACLChanged(ctx, key.AccountID)
	return nil
}

// sourceSnapshot is the audit payload for a source: its kind and every
// pipe it carries, so a destroy row records what went with it.
func sourceSnapshot(src db.Source, userPipes []db.Pipe) []byte {
	pipes := append([]string{}, src.Pipes()...)
	for _, p := range userPipes {
		pipes = append(pipes, p.Name)
	}
	b, _ := json.Marshal(struct {
		Kind  string   `json:"kind"`
		Pipes []string `json:"pipes"`
	}{string(src.Kind), pipes})
	return b
}

// purgeCollaredPipe empties one of src's pipes — auto-provisioned or
// user-created — keeping the pipe itself.
func (s *Server) purgeCollaredPipe(ctx context.Context, key db.APIKey, src db.Source, name string) (uint64, *cliproto.Error) {
	if !src.IsAutoPipe(name) {
		if _, err := db.GetPipeByName(ctx, s.Pool, src.ID, name); errors.Is(err, db.ErrNotFound) {
			return 0, cliproto.NewPipeNotFound(name, src.Handle)
		} else if err != nil {
			return 0, internalErr(err.Error())
		}
	}
	return s.purgeStream(ctx, key, src.Manifold, src.Handle, name)
}

func (s *Server) purgeUncollaredPipe(ctx context.Context, key db.APIKey, manifold, name string) (uint64, *cliproto.Error) {
	if e := validateManifold(manifold); e != nil {
		return 0, e
	}
	exists, err := db.UncollaredPipeExists(ctx, s.Pool, key.AccountID, manifold, name)
	if err != nil {
		return 0, internalErr(err.Error())
	}
	if !exists {
		return 0, cliproto.NewUncollaredPipeNotFound(name, manifold)
	}
	return s.purgeStream(ctx, key, manifold, "", name)
}

// purgeStream deletes every message in a pipe's stream. The stream, its
// retention and its sequence numbers survive, so readers' cursors stay
// valid and simply find nothing left to read. A pipe whose stream was
// never provisioned has nothing to purge.
func (s *Server) purgeStream(ctx context.Context, key db.APIKey, manifold, handle, name string) (uint64, *cliproto.Error) {
	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		return 0, internalErr("account: " + err.Error())
	}
	stream, err := js.Stream(ctx, natsubj.BuildStreamName(key.AccountID, manifold, handle, name))
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, internalErr(err.Error())
	}
	var n uint64
	if info, err := stream.Info(ctx); err == nil {
		n = info.State.Msgs
	}
	if err := stream.Purge(ctx); err != nil {
		return 0, internalErr(err.Error())
	}
	before, _ := json.Marshal(map[string]uint64{"msgs": n})
	s.auditPipe(ctx, key, db.AuditActionPipePurge, cliproto.FormatPipePath(manifold, handle, name), before, nil)
	return n, nil
}
