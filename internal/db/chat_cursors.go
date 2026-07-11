package db

import (
	"context"

	"github.com/google/uuid"
)

// ChatCursorKey composes the (kind, target) window key the read-cursor table is
// indexed by, so callers build map keys the same way the roster lookup does.
func ChatCursorKey(kind, target string) string {
	return kind + "\x00" + target
}

// ListChatReadCursors returns the user's read position for every window in the
// account as a map keyed by ChatCursorKey(kind, target) -> last_read_seq. Absent
// windows (never opened) are simply missing from the map (treated as seq 0 by
// the caller). One query feeds the whole roster's unread badges.
func ListChatReadCursors(ctx context.Context, p *Pool, accountID, userID uuid.UUID) (map[string]int64, error) {
	rows, err := p.Query(ctx,
		`SELECT kind, target, last_read_seq
		   FROM chat_read_cursors WHERE account_id = $1 AND user_id = $2`,
		accountID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var kind, target string
		var seq int64
		if err := rows.Scan(&kind, &target, &seq); err != nil {
			return nil, err
		}
		out[ChatCursorKey(kind, target)] = seq
	}
	return out, rows.Err()
}

// UpsertChatReadCursor advances the user's read position for one window to seq.
// GREATEST(existing, excluded) means the cursor only ever moves forward, so a
// stale/late write can't rewind a read position past what the user has seen.
func UpsertChatReadCursor(ctx context.Context, p *Pool, accountID, userID uuid.UUID, kind, target string, seq int64) error {
	_, err := p.Exec(ctx,
		`INSERT INTO chat_read_cursors (account_id, user_id, kind, target, last_read_seq, updated_at)
		 VALUES ($1, $2, $3, $4, $5, now())
		 ON CONFLICT (account_id, user_id, kind, target)
		 DO UPDATE SET last_read_seq = GREATEST(chat_read_cursors.last_read_seq, EXCLUDED.last_read_seq),
		               updated_at = now()`,
		accountID, userID, kind, target, seq)
	return err
}
