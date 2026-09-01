package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Audit actions. Dotted <noun>.<verb> so later writers (key.revoke,
// member.remove) read consistently alongside these. The strings are
// persisted on every row and the GUI labels/filters on them, so they're
// a stable contract — rename one and old rows become unreadable.
const (
	AuditActionPipeCreate  = "pipe.create"
	AuditActionPipeSet     = "pipe.set"
	AuditActionPipeDestroy = "pipe.destroy"

	// ACL changes. An access-control edit is arguably the thing most
	// worth auditing in the product: its entire purpose is to alter who
	// can see what. docs/ACL.md listed this as a non-goal only because
	// no trail existed when that was written.
	AuditActionACLGrant   = "acl.grant"
	AuditActionACLRevoke  = "acl.revoke"
	AuditActionACLEnforce = "acl.enforce"

	// Source lifecycle. A `source destroy` CASCADEs away every pipe
	// under the source, which makes it the most destructive single
	// operation in the product — and until now the only trace it left
	// was the sudden absence of pipe rows nobody had destroyed.
	AuditActionSourceCreate  = "source.create"
	AuditActionSourceDestroy = "source.destroy"
	// AuditActionSourcePromote is the message→pty flip (ensure-pty).
	// Recorded on the STATE CHANGE, not the request: bare `ppz terminal
	// share` calls ensure-pty on every invocation, so auditing the call
	// would bury an org's real history under one row per share.
	AuditActionSourcePromote = "source.promote"

	// Key lifecycle — who can act as the org at all.
	AuditActionKeyCreate = "key.create"
	AuditActionKeyRevoke = "key.revoke"

	// Membership. member.role is here alongside add/remove because
	// promotion to admin hands someone the key, membership and
	// ACL-enforcement gates in one POST; it is the membership edit a
	// reviewer will look for first.
	AuditActionMemberAdd    = "member.add"
	AuditActionMemberRemove = "member.remove"
	AuditActionMemberRole   = "member.role"

	// Service accounts (ACL Phase 1). A service account is precisely a
	// way for a person's action to stop looking like theirs — the key
	// acts_as the bot — so its lifecycle has to be on the record.
	AuditActionSvcCreate  = "svc.create"
	AuditActionSvcDestroy = "svc.destroy"
	AuditActionSvcKeyMint = "svc.key.mint"

	// Invites. create/revoke are the owner's actions, accept/decline the
	// invitee's; all four are filed against the ORG's trail (whose
	// membership changed) but attributed to whoever actually acted.
	AuditActionInviteCreate  = "invite.create"
	AuditActionInviteRevoke  = "invite.revoke"
	AuditActionInviteAccept  = "invite.accept"
	AuditActionInviteDecline = "invite.decline"
)

// Audit target types. Names what Target refers to.
const (
	AuditTargetPipe = "pipe"
	// AuditTargetOrg is for changes to the org itself rather than to a
	// pipe within it — the enforcement switch names the account, and
	// filing it under a pipe that does not exist would make the trail
	// lie about what was touched.
	AuditTargetOrg = "org"
	// AuditTargetSource distinguishes a handle from a pipe of the same
	// name. "chat" is a legal source handle AND a legal uncollared pipe
	// name, so without this the trail cannot say which one was
	// destroyed.
	AuditTargetSource = "source"
	AuditTargetKey    = "key"
	// AuditTargetUser covers membership changes. Target is the
	// USERNAME, not the member row's uuid — a trail you have to join
	// against `users` to read is a trail nobody reads.
	AuditTargetUser    = "user"
	AuditTargetService = "service"
	AuditTargetInvite  = "invite"
)

// AuditEvent is one append-only row of the trail. See
// migrations/0006_audit_events.sql for the column-level rationale.
type AuditEvent struct {
	ID          uuid.UUID
	AccountID   uuid.UUID
	ActorUserID uuid.UUID
	// ActorAPIKeyID is the key the action came through; nil means it came
	// from a web session instead. Keeping this distinct is what stops a
	// shared-key change from looking like a person acting in the GUI —
	// on the API path the server only knows the key's creator.
	ActorAPIKeyID *uuid.UUID
	Action        string
	TargetType    string
	Target        string
	// Before and After are raw jsonb. Either may be nil: a create has no
	// before, a destroy has no after.
	Before    []byte
	After     []byte
	CreatedAt time.Time
}

// InsertAuditEvent appends one row. ID and CreatedAt are stamped here
// when the caller left them zero, so call sites stay short.
func InsertAuditEvent(ctx context.Context, p *Pool, ev AuditEvent) error {
	if ev.ID == uuid.Nil {
		ev.ID = uuid.New()
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
	_, err := p.Exec(ctx,
		`INSERT INTO audit_events (id, account_id, actor_user_id, actor_api_key_id, action, target_type, target, before, after, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		ev.ID, ev.AccountID, ev.ActorUserID, ev.ActorAPIKeyID, ev.Action, ev.TargetType, ev.Target, ev.Before, ev.After, ev.CreatedAt)
	return err
}

// ListAuditEventsForAccount returns the account's trail newest-first,
// capped at limit. Bounded because a long-lived org's trail is unbounded
// (no retention policy yet) and the tab renders every row it's handed.
func ListAuditEventsForAccount(ctx context.Context, p *Pool, accountID uuid.UUID, limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := p.Query(ctx,
		`SELECT id, account_id, actor_user_id, actor_api_key_id, action, target_type, target, before, after, created_at
		   FROM audit_events WHERE account_id = $1
		   ORDER BY created_at DESC, id DESC
		   LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var ev AuditEvent
		if err := rows.Scan(&ev.ID, &ev.AccountID, &ev.ActorUserID, &ev.ActorAPIKeyID,
			&ev.Action, &ev.TargetType, &ev.Target, &ev.Before, &ev.After, &ev.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
