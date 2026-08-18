package server

// Scheduled sends (docs/specs/schedule.md) — the REST surface backing
// `ppz send --at/--every/--cron` and `ppz schedule {ls|rm}`:
//
//	POST   /api/v1/schedules        create (daemon forwards the resolved target)
//	GET    /api/v1/schedules        list the org's live schedules
//	DELETE /api/v1/schedules/{id}   remove by short id
//
// The daemon already ran send-grade target resolution (source/stream
// existence), but this route is the trust boundary for durable rows —
// any bearer can POST directly — so the server re-validates everything
// it stores: names (a bad handle/manifold would build a malformed or
// wildcard NATS subject at fire time), kind/spec/tz, and the payload
// cap as the FIRED envelope will carry it.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
	"github.com/pipescloud/ppz/internal/schedule"
)

// atSkewGrace is how far in the past a kind=at instant may be at
// create time. The CLI validates strictly-future against ITS clock;
// network latency + clock skew can put a legitimate `--at +2s` in the
// server's past by the time the request lands. 30s mirrors the JWT
// nbf-backdating precedent. Slightly-past instants fire on the next
// scheduler tick.
const atSkewGrace = 30 * time.Second

// resolveScheduleRow is the pure request→row step of
// handleCreateSchedule: validation and next-fire computation, `now`
// injected for testability. It returns the row ready for
// db.InsertSchedule (ID/CreatorUsername unset).
func resolveScheduleRow(req cliproto.ScheduleServerCreateRequest, key db.APIKey, now time.Time) (db.Schedule, *cliproto.Error) {
	if req.Handle != "" {
		if err := natsubj.ValidateHandle(req.Handle); err != nil {
			return db.Schedule{}, cliproto.New(cliproto.EInvalidHandle)
		}
	}
	if req.Manifold != "" {
		for _, seg := range strings.Split(req.Manifold, ".") {
			if err := natsubj.ValidateHandle(seg); err != nil {
				return db.Schedule{}, &cliproto.Error{Code: cliproto.EInvalidManifold, Message: "manifold segment invalid: " + seg}
			}
		}
	}
	if err := natsubj.ValidatePipe(req.Pipe); err != nil {
		return db.Schedule{}, cliproto.New(cliproto.EInvalidPipe)
	}
	// Size gate: probe with the shape the SCHEDULER will publish —
	// including a schedule_id of the id8 width — so a payload that
	// passes creation can never exceed MaxBytes at fire time (which
	// would fail every fire; PR #139 finding #3).
	probe := envelope.New(req.Sender, "", req.Payload, now)
	probe.ScheduleID = "aaaaaaaa"
	if data, err := probe.Marshal(); err != nil || len(data) > envelope.MaxBytes {
		return db.Schedule{}, cliproto.New(cliproto.EPayloadTooLarge)
	}

	row := db.Schedule{
		AccountID:       key.AccountID,
		Manifold:        req.Manifold,
		SourceHandle:    req.Handle,
		Pipe:            req.Pipe,
		Payload:         req.Payload,
		Sender:          req.Sender,
		Kind:            req.Kind,
		TZ:              req.TZ,
		CreatedByUserID: key.Actor(),
		CreatedAt:       now,
	}
	switch schedule.Kind(req.Kind) {
	case schedule.KindAt:
		t, err := time.Parse(time.RFC3339, req.At)
		if err != nil {
			return db.Schedule{}, &cliproto.Error{Code: cliproto.EInvalidSchedule, Message: "invalid at: " + err.Error()}
		}
		if t.Before(now.Add(-atSkewGrace)) {
			return db.Schedule{}, &cliproto.Error{Code: cliproto.EInvalidSchedule, Message: "at is in the past"}
		}
		row.Spec = req.At // display-shaped: the creator's offset survives
		row.NextFireAt = t.UTC()
	case schedule.KindEvery:
		d, err := schedule.ParseEvery(req.Every)
		if err != nil {
			return db.Schedule{}, &cliproto.Error{Code: cliproto.EInvalidSchedule, Message: "invalid every: " + err.Error()}
		}
		row.Spec = req.Every
		row.NextFireAt = now.Add(d) // grid anchor = created_at
	case schedule.KindCron:
		if err := schedule.ParseCron(req.Cron); err != nil {
			return db.Schedule{}, &cliproto.Error{Code: cliproto.EInvalidSchedule, Message: "invalid cron: " + err.Error()}
		}
		loc, err := time.LoadLocation(req.TZ)
		if err != nil {
			return db.Schedule{}, &cliproto.Error{Code: cliproto.EInvalidSchedule, Message: "invalid tz: " + req.TZ}
		}
		next, ok := schedule.NextAfter(schedule.Spec{Kind: schedule.KindCron, Cron: req.Cron, Loc: loc}, now)
		if !ok {
			return db.Schedule{}, &cliproto.Error{Code: cliproto.EInvalidSchedule, Message: "cron expression never fires"}
		}
		row.Spec = req.Cron
		row.NextFireAt = next.UTC()
	default:
		return db.Schedule{}, &cliproto.Error{Code: cliproto.EInvalidSchedule, Message: "kind must be at, every, or cron"}
	}
	return row, nil
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	var req cliproto.ScheduleServerCreateRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, &cliproto.Error{Code: cliproto.EInvalidSchedule, Message: "malformed json"})
		return
	}
	row, e := resolveScheduleRow(req, key, time.Now().UTC())
	if e != nil {
		writeErr(w, e)
		return
	}

	ctx, cancel := withTimeout(r)
	defer cancel()
	ins, err := db.InsertSchedule(ctx, s.Pool, row)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cliproto.ScheduleCreateReply{
		ID:     ins.ShortID(),
		Target: cliproto.FormatPipePath(ins.Manifold, ins.SourceHandle, ins.Pipe),
		NextAt: ins.NextFireAt,
	})
}

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	rows, err := db.ListSchedules(ctx, s.Pool, key.AccountID)
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	reply := cliproto.ScheduleListReply{Schedules: make([]cliproto.ScheduleInfo, 0, len(rows))}
	for _, row := range rows {
		reply.Schedules = append(reply.Schedules, cliproto.ScheduleInfo{
			ID:        row.ShortID(),
			Namespace: row.Manifold,
			Handle:    row.SourceHandle,
			Pipe:      row.Pipe,
			Kind:      row.Kind,
			Spec:      row.Spec,
			TZ:        row.TZ,
			NextAt:    row.NextFireAt,
			LastAt:    row.LastFiredAt,
			Payload:   row.Payload,
			Creator:   row.CreatorUsername,
		})
	}
	writeJSON(w, http.StatusOK, reply)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request, key db.APIKey) {
	id := strings.ToLower(r.PathValue("id"))
	ctx, cancel := withTimeout(r)
	defer cancel()
	ok, err := db.DeleteScheduleByShortID(ctx, s.Pool, key.AccountID, id)
	if errors.Is(err, db.ErrScheduleIDAmbiguous) {
		writeErr(w, &cliproto.Error{Code: cliproto.EInvalidSchedule,
			Message: "short id '" + id + "' matches multiple schedules (rare id collision); nothing was removed"})
		return
	}
	if err != nil {
		writeErr(w, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	if !ok {
		writeErr(w, cliproto.NewScheduleNotFound(id))
		return
	}
	writeJSON(w, http.StatusOK, cliproto.ScheduleRemoveReply{ID: id})
}
