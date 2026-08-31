package db

// ACL grant storage — ACL Phase 2.
//
// Every row is an allow that widens a derived default. See
// internal/acl for the evaluator and docs/ACL.md for the model.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pipescloud/ppz/internal/acl"
)

// ACLGrant is one stored row, joined to the principal's username so
// callers can render provenance without a second lookup.
type ACLGrant struct {
	ID            uuid.UUID
	AccountID     uuid.UUID
	PrincipalID   uuid.UUID
	PrincipalName string
	Selector      string
	Perm          string
	GrantedBy     uuid.UUID
	GrantedByName string
	CreatedAt     time.Time
}

// ToACL converts a stored row into the evaluator's shape.
func (g ACLGrant) ToACL() acl.Grant {
	var p acl.Perm
	switch g.Perm {
	case "read":
		p = acl.Read
	case "write":
		p = acl.Write
	case "admin":
		p = acl.Admin
	}
	return acl.Grant{
		PrincipalID: g.PrincipalID,
		Principal:   g.PrincipalName,
		Selector:    acl.Selector(g.Selector),
		Perm:        p,
		GrantedBy:   g.GrantedByName,
		CreatedAt:   g.CreatedAt,
	}
}

const aclGrantColumns = `
	g.id, g.account_id, g.principal_id, p.username, g.selector, g.perm,
	g.granted_by, COALESCE(b.username, ''), g.created_at`

func scanACLGrants(rows pgx.Rows) ([]ACLGrant, error) {
	defer rows.Close()
	out := []ACLGrant{}
	for rows.Next() {
		var g ACLGrant
		if err := rows.Scan(&g.ID, &g.AccountID, &g.PrincipalID, &g.PrincipalName,
			&g.Selector, &g.Perm, &g.GrantedBy, &g.GrantedByName, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListACLGrants returns every grant in the account.
func ListACLGrants(ctx context.Context, p *Pool, accountID uuid.UUID) ([]ACLGrant, error) {
	rows, err := p.Query(ctx,
		`SELECT `+aclGrantColumns+`
		   FROM acl_grants g
		   JOIN users p ON p.id = g.principal_id
		   LEFT JOIN users b ON b.id = g.granted_by
		  WHERE g.account_id = $1
		  ORDER BY g.selector, p.username, g.perm`, accountID)
	if err != nil {
		return nil, err
	}
	return scanACLGrants(rows)
}

// ListACLGrantsForPrincipal returns the grants that could apply to one
// principal — its own rows plus every @everyone row.
func ListACLGrantsForPrincipal(ctx context.Context, p *Pool, accountID, principalID uuid.UUID) ([]ACLGrant, error) {
	rows, err := p.Query(ctx,
		`SELECT `+aclGrantColumns+`
		   FROM acl_grants g
		   JOIN users p ON p.id = g.principal_id
		   LEFT JOIN users b ON b.id = g.granted_by
		  WHERE g.account_id = $1 AND g.principal_id = ANY($2::uuid[])
		  ORDER BY g.selector, g.perm`,
		accountID, []uuid.UUID{principalID, acl.EveryoneID})
	if err != nil {
		return nil, err
	}
	return scanACLGrants(rows)
}

// InsertACLGrant adds a grant. Idempotent — regranting the same
// (principal, selector, perm) is a no-op rather than an error, so a
// retrying agent doesn't see a spurious failure.
func InsertACLGrant(ctx context.Context, p *Pool, accountID, principalID uuid.UUID, selector, perm string, grantedBy uuid.UUID) error {
	if perm != "read" && perm != "write" && perm != "admin" {
		return errors.New("perm must be read, write or admin")
	}
	if !acl.ValidSelector(acl.Selector(selector)) {
		return errors.New("invalid selector: " + selector)
	}
	_, err := p.Exec(ctx,
		`INSERT INTO acl_grants (id, account_id, principal_id, selector, perm, granted_by)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (account_id, principal_id, selector, perm) DO NOTHING`,
		uuid.New(), accountID, principalID, selector, perm, grantedBy)
	if err != nil {
		return err
	}
	return BumpACLGeneration(ctx, p, accountID)
}

// DeleteACLGrant removes a grant. Idempotent: removing something that
// isn't there leaves the desired state in place, so it returns nil.
// Passing an empty perm removes every perm for that (principal,
// selector).
func DeleteACLGrant(ctx context.Context, p *Pool, accountID, principalID uuid.UUID, selector, perm string) error {
	var err error
	if perm == "" {
		_, err = p.Exec(ctx,
			`DELETE FROM acl_grants
			  WHERE account_id = $1 AND principal_id = $2 AND selector = $3`,
			accountID, principalID, selector)
	} else {
		_, err = p.Exec(ctx,
			`DELETE FROM acl_grants
			  WHERE account_id = $1 AND principal_id = $2 AND selector = $3 AND perm = $4`,
			accountID, principalID, selector, perm)
	}
	if err != nil {
		return err
	}
	return BumpACLGeneration(ctx, p, accountID)
}

// BumpACLGeneration marks the account's access state as changed. Phase
// 3 uses it to invalidate minted NATS credentials — NATS evaluates
// permissions only at connect, so a revoke needs an explicit nudge to
// reach a live connection.
func BumpACLGeneration(ctx context.Context, p *Pool, accountID uuid.UUID) error {
	_, err := p.Exec(ctx,
		`UPDATE accounts SET acl_generation = acl_generation + 1 WHERE id = $1`, accountID)
	return err
}

// ACLGeneration reads the current generation counter.
func ACLGeneration(ctx context.Context, p *Pool, accountID uuid.UUID) (int64, error) {
	var gen int64
	err := p.QueryRow(ctx,
		`SELECT acl_generation FROM accounts WHERE id = $1`, accountID).Scan(&gen)
	return gen, err
}

// ResolvePrincipal maps a name typed at the CLI to a principal in the
// account. Accepts a bare human username, a bare service-account name,
// or "@everyone".
func ResolvePrincipal(ctx context.Context, p *Pool, accountID uuid.UUID, orgName, name string) (User, error) {
	if name == "@everyone" || name == "everyone" {
		return GetUser(ctx, p, acl.EveryoneID)
	}
	// Service accounts first: their bare name is scoped in storage, so
	// a human and a service could otherwise both answer to "builder".
	if svc, err := GetServiceAccount(ctx, p, accountID, orgName, name); err == nil {
		return svc, nil
	}
	u, err := GetUserByUsername(ctx, p, name)
	if err != nil {
		return User{}, ErrNotFound
	}
	return u, nil
}

// ACLEnforced reports whether the org enforces ACLs. False for every
// org until an admin opts in.
func ACLEnforced(ctx context.Context, p *Pool, accountID uuid.UUID) (bool, error) {
	var on bool
	err := p.QueryRow(ctx,
		`SELECT acl_enforced FROM accounts WHERE id = $1`, accountID).Scan(&on)
	return on, err
}

// SetACLEnforced flips the switch and bumps the generation counter, so
// live daemons re-exchange and pick up (or drop) compiled credentials
// without a restart — the same invalidation path a revoke uses.
//
// Non-destructive in both directions: grant rows are untouched, so
// disabling and re-enabling restores the previous configuration exactly.
func SetACLEnforced(ctx context.Context, p *Pool, accountID uuid.UUID, on bool) error {
	if _, err := p.Exec(ctx,
		`UPDATE accounts SET acl_enforced = $2 WHERE id = $1`, accountID, on); err != nil {
		return err
	}
	return BumpACLGeneration(ctx, p, accountID)
}
