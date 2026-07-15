package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SourceKind enumerates the supported source shapes.
//
//	"message" — default; two pipes: broadcast, inbox.
//	"pty"     — terminal source; broadcast, inbox, and terminal IO pipes.
type SourceKind string

const (
	SourceKindMessage SourceKind = "message"
	SourceKindPTY     SourceKind = "pty"
)

type Source struct {
	ID                   uuid.UUID
	AccountID            uuid.UUID
	CreatedByUserID      uuid.UUID // user that created the source (NOT NULL)
	Manifold             string    // hierarchical-grouping segment; '' = root (NOT NULL on the DB column)
	Handle               string
	Kind                 SourceKind
	CreatedAt            time.Time
	LastBroadcastAt      *time.Time
	LastBroadcastPayload *string
}

// Pipes returns the pipe set for a source based on its kind.
// All sources have:
//   - inbox: direct messages intended for this source/agent
//
// pty sources also have:
//   - stdin: input fed to the wrapped child via `ppz send`
//   - stdout: byte-faithful capture of the PTY master's output (ANSI
//     escapes intact); both `ppz read` and `ppz terminal view` consume
//     this pipe.
//   - stdctrl: control plane (resize/setsize events — device geometry).
//   - system: session control plane — write-lease acquire/release requests
//     (writer → host) and host-published lease-state events (host →
//     observers). Distinct from stdctrl: stdctrl mutates the pty device,
//     system coordinates who may write. See docs/WIRE.md.
//
// `broadcast` was an auto-provisioned pipe pre-launch; it was removed
// in Phase 1 (locked decision #16) — teams now use explicit room pipes
// (`ppz pipe create team1.room` with --writers=anyone) for shared
// channels.
func (s Source) Pipes() []string {
	switch s.Kind {
	case SourceKindPTY:
		return []string{"stdin", "stdout", "stdctrl", "system", "inbox", "heartbeat"}
	default:
		return []string{"inbox"}
	}
}

// IsAutoPipe reports whether name is an auto-provisioned pipe for this source
// kind (i.e. JetStream-only, not stored in the pipes table).
func (s Source) IsAutoPipe(name string) bool {
	for _, p := range s.Pipes() {
		if p == name {
			return true
		}
	}
	return false
}

// ErrHandleTaken is returned when a (org, handle) row already exists.
var ErrHandleTaken = errors.New("handle taken")

// InsertSource creates a row attributed to `createdBy` (NOT NULL on the
// table). Server callers stamp this from the API-key's CreatedByUserID
// (API path) or caller.UserID (OAuth path).
func InsertSource(ctx context.Context, p *Pool, accountID, createdBy uuid.UUID, manifold, handle string, kind SourceKind) (Source, error) {
	if kind == "" {
		kind = SourceKindMessage
	}
	src := Source{
		ID:              uuid.New(),
		AccountID:       accountID,
		CreatedByUserID: createdBy,
		Manifold:        manifold,
		Handle:          handle,
		Kind:            kind,
		CreatedAt:       time.Now().UTC(),
	}
	_, err := p.Exec(ctx,
		`INSERT INTO sources (id, account_id, created_by_user_id, manifold, handle, kind, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		src.ID, src.AccountID, src.CreatedByUserID, src.Manifold, src.Handle, string(src.Kind), src.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Source{}, ErrHandleTaken
		}
		return Source{}, err
	}
	return src, nil
}

func GetSourceByHandle(ctx context.Context, p *Pool, accountID uuid.UUID, handle string) (Source, error) {
	var src Source
	var kind string
	err := p.QueryRow(ctx,
		`SELECT id, account_id, created_by_user_id, manifold, handle, kind, created_at, last_broadcast_at, last_broadcast_payload
		   FROM sources WHERE account_id = $1 AND handle = $2`, accountID, handle).
		Scan(&src.ID, &src.AccountID, &src.CreatedByUserID, &src.Manifold, &src.Handle, &kind, &src.CreatedAt,
			&src.LastBroadcastAt, &src.LastBroadcastPayload)
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	src.Kind = SourceKind(kind)
	return src, err
}

// UpdateSourceKind changes an existing source's kind in place. Used by the
// "ensure pty" upgrade path when a bare `terminal share` runs against an
// inbox-only (message) source: sharing a source declares it a terminal, so
// its kind is promoted to pty and its full pipe set provisioned. `kind` is a
// plain mutable column. Returns ErrNotFound when (account, handle) is absent.
// Idempotent: setting pty on an already-pty row affects one row, no error.
func UpdateSourceKind(ctx context.Context, p *Pool, accountID uuid.UUID, handle string, kind SourceKind) error {
	tag, err := p.Exec(ctx,
		`UPDATE sources SET kind = $1 WHERE account_id = $2 AND handle = $3`,
		string(kind), accountID, handle)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func ListSourcesForOrg(ctx context.Context, p *Pool, accountID uuid.UUID) ([]Source, error) {
	rows, err := p.Query(ctx,
		`SELECT id, account_id, created_by_user_id, manifold, handle, kind, created_at, last_broadcast_at, last_broadcast_payload
		   FROM sources WHERE account_id = $1 ORDER BY manifold ASC, handle ASC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var src Source
		var kind string
		if err := rows.Scan(&src.ID, &src.AccountID, &src.CreatedByUserID, &src.Manifold, &src.Handle, &kind, &src.CreatedAt,
			&src.LastBroadcastAt, &src.LastBroadcastPayload); err != nil {
			return nil, err
		}
		src.Kind = SourceKind(kind)
		out = append(out, src)
	}
	return out, rows.Err()
}

// DeleteSource removes a source row. The pipes FK is ON DELETE CASCADE so
// pipe rows are removed automatically. JetStream stream cleanup is the
// caller's responsibility. Returns ErrNotFound when (org, handle) doesn't exist.
func DeleteSource(ctx context.Context, p *Pool, accountID uuid.UUID, handle string) error {
	tag, err := p.Exec(ctx,
		`DELETE FROM sources WHERE account_id = $1 AND handle = $2`, accountID, handle)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateLastBroadcast records the most recent broadcast for this source.
// Called by the server-side subscriber on every message. Idempotent on
// identical inputs.
func UpdateLastBroadcast(ctx context.Context, p *Pool, accountID uuid.UUID, handle string, at time.Time, payload string) error {
	_, err := p.Exec(ctx,
		`UPDATE sources SET last_broadcast_at = $1, last_broadcast_payload = $2
		   WHERE account_id = $3 AND handle = $4`,
		at, payload, accountID, handle)
	return err
}

