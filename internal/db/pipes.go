package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Pipe is one user-creatable channel. Phase 1.5: pipes carry an explicit
// manifold (hierarchical-grouping segment, '' = root) and an account_id
// (denormalised from source.account_id for collared rows, explicit for
// uncollared ones). SourceID is nullable — uncollared (sourceless) pipes
// are symmetric many-to-many channels under a manifold.
//
// Auto-provisioned pipes (stdin, stdout, stdctrl, inbox) are NOT stored
// here — they're derived from the source's kind and joined in at API
// response time.
type Pipe struct {
	ID              uuid.UUID
	AccountID       uuid.UUID  // tenancy anchor (denormalised; matches source.account_id when SourceID is set)
	Manifold        string     // hierarchical-grouping segment; '' = root (NOT NULL on the DB column)
	SourceID        *uuid.UUID // nil for uncollared (sourceless) pipes
	CreatedByUserID uuid.UUID  // user that created the pipe (NOT NULL)
	Name            string
	// Retention overrides. nil = no opinion at this layer; the server
	// resolves the field from the next layer down (account default, then
	// the built-in default) — see resolveRetention in internal/server.
	// Concrete defaults live there, not here, so they can't drift.
	TTLSeconds *int
	MaxMsgs    *int
	MaxBytes   *int64
	CreatedAt  time.Time
}

// ErrPipeNameTaken — uniqueness collision on insert. The partial UNIQUE
// indexes split this by collared/uncollared shape, but both surface here.
var ErrPipeNameTaken = errors.New("pipe name taken")

// InsertPipe inserts a row. sourceID nil = uncollared (symmetric many-to-many
// pipe under the manifold); non-nil = collared under that source. The caller
// passes accountID explicitly because uncollared rows can't derive it from
// source.account_id. Retention overrides are NULL when the pointer arg is
// nil — the server provisions the JetStream stream with defaults.
func InsertPipe(ctx context.Context, p *Pool, accountID uuid.UUID, manifold string, sourceID *uuid.UUID, createdBy uuid.UUID, name string, ttl *int, maxMsgs *int, maxBytes *int64) (Pipe, error) {
	pipe := Pipe{
		ID:              uuid.New(),
		AccountID:       accountID,
		Manifold:        manifold,
		SourceID:        sourceID,
		CreatedByUserID: createdBy,
		Name:            name,
		TTLSeconds:      ttl,
		MaxMsgs:         maxMsgs,
		MaxBytes:        maxBytes,
		CreatedAt:       time.Now().UTC(),
	}
	_, err := p.Exec(ctx,
		`INSERT INTO pipes (id, account_id, manifold, source_id, created_by_user_id, name, ttl_seconds, max_msgs, max_bytes, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		pipe.ID, pipe.AccountID, pipe.Manifold, pipe.SourceID, pipe.CreatedByUserID, pipe.Name, pipe.TTLSeconds, pipe.MaxMsgs, pipe.MaxBytes, pipe.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Pipe{}, ErrPipeNameTaken
		}
		return Pipe{}, err
	}
	return pipe, nil
}

// ListPipesForSource returns the user-creatable pipes for one source,
// sorted by name. Excludes auto-provisioned pipes (those aren't stored).
func ListPipesForSource(ctx context.Context, p *Pool, sourceID uuid.UUID) ([]Pipe, error) {
	rows, err := p.Query(ctx,
		`SELECT id, account_id, manifold, source_id, created_by_user_id, name, ttl_seconds, max_msgs, max_bytes, created_at
		   FROM pipes WHERE source_id = $1 ORDER BY name ASC`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pipe
	for rows.Next() {
		var pipe Pipe
		if err := rows.Scan(&pipe.ID, &pipe.AccountID, &pipe.Manifold, &pipe.SourceID, &pipe.CreatedByUserID, &pipe.Name,
			&pipe.TTLSeconds, &pipe.MaxMsgs, &pipe.MaxBytes, &pipe.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, pipe)
	}
	return out, rows.Err()
}

// GetPipeByName returns one pipe row or ErrNotFound.
func GetPipeByName(ctx context.Context, p *Pool, sourceID uuid.UUID, name string) (Pipe, error) {
	var pipe Pipe
	err := p.QueryRow(ctx,
		`SELECT id, account_id, manifold, source_id, created_by_user_id, name, ttl_seconds, max_msgs, max_bytes, created_at
		   FROM pipes WHERE source_id = $1 AND name = $2`, sourceID, name).
		Scan(&pipe.ID, &pipe.AccountID, &pipe.Manifold, &pipe.SourceID, &pipe.CreatedByUserID, &pipe.Name,
			&pipe.TTLSeconds, &pipe.MaxMsgs, &pipe.MaxBytes, &pipe.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pipe{}, ErrNotFound
	}
	return pipe, err
}

// GetUncollaredPipeByName returns one sourceless pipe row addressed the
// uncollared way — (account, manifold, name) — or ErrNotFound.
// GetPipeByName only covers the collared shape (keyed on source_id),
// which uncollared pipes don't have.
func GetUncollaredPipeByName(ctx context.Context, p *Pool, accountID uuid.UUID, manifold, name string) (Pipe, error) {
	var pipe Pipe
	err := p.QueryRow(ctx,
		`SELECT id, account_id, manifold, source_id, created_by_user_id, name, ttl_seconds, max_msgs, max_bytes, created_at
		   FROM pipes WHERE account_id = $1 AND manifold = $2 AND name = $3 AND source_id IS NULL`,
		accountID, manifold, name).
		Scan(&pipe.ID, &pipe.AccountID, &pipe.Manifold, &pipe.SourceID, &pipe.CreatedByUserID, &pipe.Name,
			&pipe.TTLSeconds, &pipe.MaxMsgs, &pipe.MaxBytes, &pipe.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pipe{}, ErrNotFound
	}
	return pipe, err
}

// UpdatePipeRetention writes the retention triple for an existing row.
//
// It takes the FULL triple, not a partial patch: a nil arg stores NULL
// (i.e. drops the field back to the default layer), it does not mean
// "leave the column alone". Merging a partial request onto the stored
// row belongs to the caller — the caller needs the old row anyway to
// echo the resolved retention back, and keeping the merge out of SQL
// means exactly one place decides precedence.
func UpdatePipeRetention(ctx context.Context, p *Pool, id uuid.UUID, ttl *int, maxMsgs *int, maxBytes *int64) error {
	tag, err := p.Exec(ctx,
		`UPDATE pipes SET ttl_seconds = $2, max_msgs = $3, max_bytes = $4 WHERE id = $1`,
		id, ttl, maxMsgs, maxBytes)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpsertPipeRetention stores retention for a pipe that may not have a
// row yet. This is what makes AUTO-PROVISIONED pipes (inbox, and
// stdin/stdout/stdctrl/system/heartbeat on pty sources) configurable:
// they're JetStream-only by design, so there was previously nowhere to
// persist an override — and they're precisely the pipes whose default
// caps users hit first.
//
// Materialising a row rather than adding a parallel overrides table
// keeps one resolution path (nil column = default), and every reader
// already unions auto-pipe names with table rows through a set, so a
// materialised row does not double-list in `ppz ls` or the org page.
//
// Distinct from InsertPipe because it must be reachable for reserved
// names, which InsertPipe's callers gate on. It is NOT a create path:
// callers verify the pipe exists (as a row or as an auto-pipe of the
// source) before calling.
func UpsertPipeRetention(ctx context.Context, p *Pool, accountID uuid.UUID, manifold string, sourceID *uuid.UUID, createdBy uuid.UUID, name string, ttl *int, maxMsgs *int, maxBytes *int64) (Pipe, error) {
	pipe := Pipe{
		ID:              uuid.New(),
		AccountID:       accountID,
		Manifold:        manifold,
		SourceID:        sourceID,
		CreatedByUserID: createdBy,
		Name:            name,
		TTLSeconds:      ttl,
		MaxMsgs:         maxMsgs,
		MaxBytes:        maxBytes,
		CreatedAt:       time.Now().UTC(),
	}
	// The uniqueness constraints are partial indexes split by shape
	// (collared vs uncollared), so there is no single ON CONFLICT target
	// that covers both. Try the update first, insert when it matches
	// nothing.
	var tag pgconn.CommandTag
	var err error
	if sourceID != nil {
		tag, err = p.Exec(ctx,
			`UPDATE pipes SET ttl_seconds = $3, max_msgs = $4, max_bytes = $5
			   WHERE source_id = $1 AND name = $2`,
			*sourceID, name, ttl, maxMsgs, maxBytes)
	} else {
		tag, err = p.Exec(ctx,
			`UPDATE pipes SET ttl_seconds = $4, max_msgs = $5, max_bytes = $6
			   WHERE account_id = $1 AND manifold = $2 AND name = $3 AND source_id IS NULL`,
			accountID, manifold, name, ttl, maxMsgs, maxBytes)
	}
	if err != nil {
		return Pipe{}, err
	}
	if tag.RowsAffected() > 0 {
		if sourceID != nil {
			return GetPipeByName(ctx, p, *sourceID, name)
		}
		return GetUncollaredPipeByName(ctx, p, accountID, manifold, name)
	}

	_, err = p.Exec(ctx,
		`INSERT INTO pipes (id, account_id, manifold, source_id, created_by_user_id, name, ttl_seconds, max_msgs, max_bytes, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		pipe.ID, pipe.AccountID, pipe.Manifold, pipe.SourceID, pipe.CreatedByUserID, pipe.Name,
		pipe.TTLSeconds, pipe.MaxMsgs, pipe.MaxBytes, pipe.CreatedAt)
	if err != nil {
		return Pipe{}, err
	}
	return pipe, nil
}

// DeletePipe removes the row. Returns ErrNotFound when (source, name) doesn't
// exist. Stream cleanup is the caller's responsibility (server-side).
func DeletePipe(ctx context.Context, p *Pool, sourceID uuid.UUID, name string) error {
	tag, err := p.Exec(ctx,
		`DELETE FROM pipes WHERE source_id = $1 AND name = $2`, sourceID, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUncollaredPipe removes an uncollared pipe row by (account, manifold,
// name). Stream cleanup is the caller's responsibility. Phase 1.5.
func DeleteUncollaredPipe(ctx context.Context, p *Pool, accountID uuid.UUID, manifold, name string) error {
	tag, err := p.Exec(ctx,
		`DELETE FROM pipes WHERE account_id = $1 AND manifold = $2 AND name = $3 AND source_id IS NULL`,
		accountID, manifold, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UncollaredPipeExists reports whether a sourceless pipe with the given
// (account, manifold, name) already exists. Phase 1.5.1 collision check —
// source creation rejects when an uncollared pipe shares the source's
// proposed name at the same manifold.
// UserHasAnyPipe reports whether the user owns or is a member of any
// account that has at least one pipe (collared or uncollared). Powers
// the dashboard onboarding panel: when this is false, the empty-state
// get-started instructions render; once the user creates a pipe the
// panel hides itself.
func UserHasAnyPipe(ctx context.Context, p *Pool, userID uuid.UUID) (bool, error) {
	var n int
	err := p.QueryRow(ctx, `
		SELECT 1
		  FROM pipes pp
		  JOIN accounts a ON pp.account_id = a.id
		  LEFT JOIN account_members m ON m.account_id = a.id AND m.user_id = $1
		 WHERE a.owner_user_id = $1 OR m.user_id IS NOT NULL
		 LIMIT 1`, userID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func UncollaredPipeExists(ctx context.Context, p *Pool, accountID uuid.UUID, manifold, name string) (bool, error) {
	var n int
	err := p.QueryRow(ctx,
		`SELECT 1 FROM pipes WHERE account_id = $1 AND manifold = $2 AND name = $3 AND source_id IS NULL LIMIT 1`,
		accountID, manifold, name).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// PipesExistAtManifold reports whether ANY pipe (collared or uncollared)
// exists at the given manifold prefix. Phase 1.5.1 collision check — a
// new source's handle at manifold M reserves the prefix path M.<handle>
// (or just <handle> if M is empty), so source creation rejects when
// pipes already live there.
func PipesExistAtManifold(ctx context.Context, p *Pool, accountID uuid.UUID, manifold string) (bool, error) {
	var n int
	err := p.QueryRow(ctx,
		`SELECT 1 FROM pipes WHERE account_id = $1 AND manifold = $2 LIMIT 1`,
		accountID, manifold).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// SourceExistsAtManifold reports whether a source with the given handle
// exists at the manifold. Phase 1.5.1 collision check — uncollared pipe
// creation rejects when a source shares the proposed name at the same
// manifold.
func SourceExistsAtManifold(ctx context.Context, p *Pool, accountID uuid.UUID, manifold, handle string) (bool, error) {
	var n int
	err := p.QueryRow(ctx,
		`SELECT 1 FROM sources WHERE account_id = $1 AND manifold = $2 AND handle = $3 LIMIT 1`,
		accountID, manifold, handle).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListUncollaredPipesForAccount returns every uncollared pipe row in the
// account, sorted (manifold, name). Used by `ppz ls` to surface the
// sourceless rows that walking sources alone misses. Phase 1.5.
func ListUncollaredPipesForAccount(ctx context.Context, p *Pool, accountID uuid.UUID) ([]Pipe, error) {
	rows, err := p.Query(ctx,
		`SELECT id, account_id, manifold, source_id, created_by_user_id, name, ttl_seconds, max_msgs, max_bytes, created_at
		   FROM pipes WHERE account_id = $1 AND source_id IS NULL
		   ORDER BY manifold ASC, name ASC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pipe
	for rows.Next() {
		var pipe Pipe
		if err := rows.Scan(&pipe.ID, &pipe.AccountID, &pipe.Manifold, &pipe.SourceID, &pipe.CreatedByUserID, &pipe.Name,
			&pipe.TTLSeconds, &pipe.MaxMsgs, &pipe.MaxBytes, &pipe.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, pipe)
	}
	return out, rows.Err()
}
