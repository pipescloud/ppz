package db

import (
	"context"

	"github.com/google/uuid"
)

// ChatCursorKey composes the (kind, acting, target) conversation key the
// read-cursor table is indexed by, so callers build map keys the same way the
// roster lookup does. `acting` is the viewer's handle ("" for pipes / god's-eye).
func ChatCursorKey(kind, acting, target string) string {
	return kind + "\x00" + acting + "\x00" + target
}

// ListChatReadCursors returns the user's read position for every conversation in
// the account as a map keyed by ChatCursorKey(kind, acting, target) ->
// last_read_seq. Absent conversations (never opened) are missing (seq 0 to the
// caller). One query feeds the whole roster's unread badges.
func ListChatReadCursors(ctx context.Context, p *Pool, accountID, userID uuid.UUID) (map[string]int64, error) {
	rows, err := p.Query(ctx,
		`SELECT kind, acting, target, last_read_seq
		   FROM chat_read_cursors WHERE account_id = $1 AND user_id = $2`,
		accountID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var kind, acting, target string
		var seq int64
		if err := rows.Scan(&kind, &acting, &target, &seq); err != nil {
			return nil, err
		}
		out[ChatCursorKey(kind, acting, target)] = seq
	}
	return out, rows.Err()
}

// DeleteChatReadCursorsForTarget removes every read cursor (all users, all
// acting handles) for one window. Called when the underlying pipe/source is
// deleted so a same-name recreate starts fresh instead of inheriting a stale
// high last_read_seq (which would suppress the new stream's early unread).
func DeleteChatReadCursorsForTarget(ctx context.Context, p *Pool, accountID uuid.UUID, kind, target string) error {
	_, err := p.Exec(ctx,
		`DELETE FROM chat_read_cursors WHERE account_id = $1 AND kind = $2 AND target = $3`,
		accountID, kind, target)
	return err
}

// UpsertChatReadCursor advances the user's read position for one conversation to
// seq. GREATEST(existing, excluded) means the cursor only ever moves forward, so
// a stale/late write can't rewind a read position past what the user has seen.
func UpsertChatReadCursor(ctx context.Context, p *Pool, accountID, userID uuid.UUID, kind, acting, target string, seq int64) error {
	_, err := p.Exec(ctx,
		`INSERT INTO chat_read_cursors (account_id, user_id, kind, acting, target, last_read_seq, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, now())
		 ON CONFLICT (account_id, user_id, kind, acting, target)
		 DO UPDATE SET last_read_seq = GREATEST(chat_read_cursors.last_read_seq, EXCLUDED.last_read_seq),
		               updated_at = now()`,
		accountID, userID, kind, acting, target, seq)
	return err
}
