package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// `ppz pipe set` — retention mutation for an EXISTING pipe. Two
// endpoints, mirroring the create split:
//
//	PATCH /api/v1/sources/{handle}/pipes/{name}   collared
//	PATCH /api/v1/pipes                           uncollared (body-addressed)
//
// The uncollared one is body-addressed for the same reason DELETE
// /api/v1/pipes is: an uncollared pipe's address is (manifold, name), and
// a manifold is a dotted path that doesn't survive one path segment.

// mergeRetention applies a request's overrides onto a stored row.
//
// The two nil meanings differ and the difference is the whole contract:
//   - nil on the REQUEST means "leave this field alone" → keep stored
//   - nil on the STORED row means "no override" → stays nil, so the field
//     keeps resolving from the default layer
//
// Getting this backwards would make `pipe set --ttl=1h` silently wipe a
// previously configured max-msgs back to the default.
func mergeRetention(stored db.Pipe, ttl *int, maxMsgs *int, maxBytes *int64) (*int, *int, *int64) {
	outTTL := stored.TTLSeconds
	if ttl != nil {
		outTTL = ttl
	}
	outMsgs := stored.MaxMsgs
	if maxMsgs != nil {
		outMsgs = maxMsgs
	}
	outBytes := stored.MaxBytes
	if maxBytes != nil {
		outBytes = maxBytes
	}
	return outTTL, outMsgs, outBytes
}

// namesNoRetention reports a request that would change nothing. Rejected
// before any DB work: echoing back unchanged retention as though
// something happened is worse than an error.
func namesNoRetention(req cliproto.PipeSetRequest) bool {
	return req.TTLSeconds == nil && req.MaxMsgs == nil && req.MaxBytes == nil
}

// validateSettablePipeName checks name SHAPE only.
//
// Unlike `pipe create`, `pipe set` must accept reserved names: inbox and
// stdout are exactly the pipes whose default caps users hit first, and
// being auto-provisioned they can never be reached through create.
// ValidateUserPipeName would refuse them, so this uses the regex-only
// ValidatePipe — the same check `read` / `send` apply to a destination,
// where any existing pipe is legitimate.
func validateSettablePipeName(name string) bool {
	if name == "" {
		return false
	}
	return natsubj.ValidatePipe(name) == nil
}

// handleSetPipe: PATCH /api/v1/sources/{handle}/pipes/{name}
func (s *Server) handleSetPipe(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	var req cliproto.PipeSetRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, cliproto.New(cliproto.EInvalidPipe))
		return
	}
	handle := r.PathValue("handle")
	name := r.PathValue("name")
	if err := natsubj.ValidateHandle(handle); err != nil {
		writeErr(w, cliproto.NewInvalidHandle(handle))
		return
	}
	if !validateSettablePipeName(name) {
		writeErr(w, cliproto.NewInvalidPipeName(name))
		return
	}
	if err := s.requirePipeAdmin(r.Context(), key, handle+"."+name); err != nil {
		writeErr(w, cliproto.New(cliproto.EPipeForbidden))
		return
	}
	if namesNoRetention(req) {
		writeErr(w, &cliproto.Error{Code: cliproto.EInvalidPipe,
			Message: "pipe set: name at least one of --ttl, --max-msgs, --max-bytes"})
		return
	}

	ctx, cancel := withTimeout(r)
	defer cancel()

	src, err := db.GetSourceByHandle(ctx, s.Pool, key.AccountID, handle)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, cliproto.NewSourceNotFound(handle))
			return
		}
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	// The pipe must already exist — as a row, or as an auto-pipe of this
	// source. `pipe set` never creates.
	stored, err := db.GetPipeByName(ctx, s.Pool, src.ID, name)
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
			return
		}
		if !src.IsAutoPipe(name) {
			writeErr(w, cliproto.NewPipeNotFound(name, handle))
			return
		}
		// Auto-pipe with no row yet: stored stays the zero Pipe, which
		// resolves to the defaults — the retention it actually has.
		stored = db.Pipe{}
	}

	beforeAge, beforeMsgs, beforeBytes := pipeRetention(stored)
	ttl, maxMsgs, maxBytes := mergeRetention(stored, req.TTLSeconds, req.MaxMsgs, req.MaxBytes)

	// created_by is the SOURCE's creator, not the caller. This only
	// applies when materialising a row for an auto-pipe (an existing row
	// keeps its own creator, since the upsert's UPDATE leaves the column
	// alone), and `ppz ls` renders it in the CREATOR column, where
	// auto-pipes inherit the source's creator. Stamping the caller would
	// silently reassign chat.inbox to whoever last changed its cap.
	// Changing retention is not creating a pipe — who changed it is what
	// the audit trail records.
	updated, err := db.UpsertPipeRetention(ctx, s.Pool, key.AccountID, src.Manifold, &src.ID,
		src.CreatedByUserID, name, ttl, maxMsgs, maxBytes)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	age, msgs, bytes := pipeRetention(updated)
	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "org account: " + err.Error()})
		return
	}
	if err := ensurePipeStreamWithRetention(ctx, js, key.AccountID, src.Manifold, src.Handle, name, age, msgs, bytes); err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	s.auditPipe(ctx, key, db.AuditActionPipeSet,
		cliproto.FormatPipePath(src.Manifold, src.Handle, name),
		snapshotRetention(beforeAge, beforeMsgs, beforeBytes).mustJSON(),
		snapshotRetention(age, msgs, bytes).mustJSON())

	writeJSON(w, http.StatusOK, cliproto.PipeSetReply{
		Handle:     src.Handle,
		Manifold:   src.Manifold,
		Name:       name,
		StreamName: natsubj.BuildStreamName(key.AccountID, src.Manifold, src.Handle, name),
		TTLSeconds: int(age / time.Second),
		MaxMsgs:    msgs,
		MaxBytes:   bytes,
	})
}

// handleSetPipeFullPath: PATCH /api/v1/pipes — the uncollared form.
func (s *Server) handleSetPipeFullPath(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	var req cliproto.PipeSetRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, cliproto.New(cliproto.EInvalidPipe))
		return
	}
	if req.Handle != "" || (req.SourceHandle != nil && *req.SourceHandle != "") {
		writeErr(w, &cliproto.Error{Code: cliproto.EInvalidPipe,
			Message: "PATCH /api/v1/pipes is the uncollared (sourceless) endpoint; for collared pipes use PATCH /api/v1/sources/{handle}/pipes/{name}"})
		return
	}
	if !validateSettablePipeName(req.Name) {
		writeErr(w, cliproto.NewInvalidPipeName(req.Name))
		return
	}
	if req.Manifold != "" {
		for _, seg := range strings.Split(req.Manifold, ".") {
			if err := natsubj.ValidateHandle(seg); err != nil {
				writeErr(w, &cliproto.Error{Code: cliproto.EInvalidManifold, Message: "manifold segment invalid: " + seg})
				return
			}
		}
	}
	uncollaredPath := req.Name
	if req.Manifold != "" {
		uncollaredPath = req.Manifold + "." + req.Name
	}
	if err := s.requirePipeAdmin(r.Context(), key, uncollaredPath); err != nil {
		writeErr(w, cliproto.New(cliproto.EPipeForbidden))
		return
	}
	if namesNoRetention(req) {
		writeErr(w, &cliproto.Error{Code: cliproto.EInvalidPipe,
			Message: "pipe set: name at least one of --ttl, --max-msgs, --max-bytes"})
		return
	}

	ctx, cancel := withTimeout(r)
	defer cancel()

	stored, err := db.GetUncollaredPipeByName(ctx, s.Pool, key.AccountID, req.Manifold, req.Name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// Uncollared pipes have no auto-pipe set to fall back on —
			// no row means no pipe.
			writeErr(w, cliproto.NewUncollaredPipeNotFound(req.Name, req.Manifold))
			return
		}
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	beforeAge, beforeMsgs, beforeBytes := pipeRetention(stored)
	ttl, maxMsgs, maxBytes := mergeRetention(stored, req.TTLSeconds, req.MaxMsgs, req.MaxBytes)

	if err := db.UpdatePipeRetention(ctx, s.Pool, stored.ID, ttl, maxMsgs, maxBytes); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, cliproto.NewUncollaredPipeNotFound(req.Name, req.Manifold))
			return
		}
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	stored.TTLSeconds, stored.MaxMsgs, stored.MaxBytes = ttl, maxMsgs, maxBytes
	age, msgs, bytes := pipeRetention(stored)

	js, err := s.JSFor(ctx, key.AccountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: "account: " + err.Error()})
		return
	}
	if err := ensurePipeStreamWithRetention(ctx, js, key.AccountID, req.Manifold, "", req.Name, age, msgs, bytes); err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}

	s.auditPipe(ctx, key, db.AuditActionPipeSet,
		cliproto.FormatPipePath(req.Manifold, "", req.Name),
		snapshotRetention(beforeAge, beforeMsgs, beforeBytes).mustJSON(),
		snapshotRetention(age, msgs, bytes).mustJSON())

	writeJSON(w, http.StatusOK, cliproto.PipeSetReply{
		Handle:     "",
		Manifold:   req.Manifold,
		Name:       req.Name,
		StreamName: natsubj.BuildStreamName(key.AccountID, req.Manifold, "", req.Name),
		TTLSeconds: int(age / time.Second),
		MaxMsgs:    msgs,
		MaxBytes:   bytes,
	})
}
