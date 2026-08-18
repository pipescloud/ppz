package db

// Service accounts — ACL Phase 1.
//
// An agent's own identity, distinct from the human who spawned it: a
// real principal that holds ACL grants, owns handles, and is attributed
// on `ppz who` and on every message it publishes. It differs from a
// human only in how it authenticates (key only, no OAuth) and in being
// scoped to exactly one account.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrServiceExists is returned when a name is already taken in the org.
var ErrServiceExists = errors.New("service account already exists")

// DisplayName is the name every surface shows. Service accounts store
// an org-scoped username ("<org>/<name>") because users.username is
// globally unique; users see the bare name.
func (u User) DisplayName() string {
	if _, name, ok := ParseServiceUsername(u.Username); ok && u.IsService() {
		return name
	}
	return u.Username
}

// InsertServiceAccount creates a service principal in the given org.
func InsertServiceAccount(ctx context.Context, p *Pool, accountID uuid.UUID, orgName, name string) (User, error) {
	if _, err := ValidateServiceName(name); err != nil {
		return User{}, err
	}
	acct := accountID
	u := User{
		ID:               uuid.New(),
		Username:         ServiceUsername(orgName, name),
		Email:            ServiceUsername(orgName, name) + "@service.local",
		Mode:             UserModeService,
		ServiceAccountID: &acct,
		CreatedAt:        time.Now().UTC(),
	}
	_, err := p.Exec(ctx,
		`INSERT INTO users (id, username, email, mode, service_account_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		u.ID, u.Username, u.Email, string(u.Mode), acct, u.CreatedAt)
	if err != nil {
		// 23505 = unique_violation on users.username.
		if isUniqueViolation(err) {
			return User{}, ErrServiceExists
		}
		return User{}, err
	}
	// A service account is a member of its own org so RoleInOrg
	// resolves it — without this it would be OrgRoleNone and the ACL
	// evaluator would deny it everything, including its own inbox.
	if err := AddMember(ctx, p, accountID, u.ID); err != nil {
		return User{}, fmt.Errorf("add service account to org: %w", err)
	}
	return u, nil
}

// ListServiceAccounts returns every service principal in the org.
func ListServiceAccounts(ctx context.Context, p *Pool, accountID uuid.UUID) ([]User, error) {
	rows, err := p.Query(ctx,
		`SELECT id, username, email, mode, service_account_id, created_at
		   FROM users
		  WHERE service_account_id = $1 AND mode = 'service'
		  ORDER BY username ASC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var mode string
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &mode, &u.ServiceAccountID, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Mode = UserMode(mode)
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetServiceAccount resolves a bare name within an org.
func GetServiceAccount(ctx context.Context, p *Pool, accountID uuid.UUID, orgName, name string) (User, error) {
	var u User
	var mode string
	err := p.QueryRow(ctx,
		`SELECT id, username, email, mode, service_account_id, created_at
		   FROM users
		  WHERE service_account_id = $1 AND username = $2 AND mode = 'service'`,
		accountID, ServiceUsername(orgName, name)).
		Scan(&u.ID, &u.Username, &u.Email, &mode, &u.ServiceAccountID, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	u.Mode = UserMode(mode)
	return u, err
}

// DeleteServiceAccount removes the principal. Its keys and ACL grants
// cascade from the users FK.
func DeleteServiceAccount(ctx context.Context, p *Pool, accountID uuid.UUID, orgName, name string) error {
	tag, err := p.Exec(ctx,
		`DELETE FROM users
		  WHERE service_account_id = $1 AND username = $2 AND mode = 'service'`,
		accountID, ServiceUsername(orgName, name))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMemberRole sets a member's role within an org. 'owner' is not
// settable here — ownership lives on accounts.owner_user_id and moves
// only through the transfer flow.
func SetMemberRole(ctx context.Context, p *Pool, accountID, userID uuid.UUID, role string) error {
	if role != "admin" && role != "member" {
		return fmt.Errorf("role must be 'admin' or 'member', got %q", role)
	}
	tag, err := p.Exec(ctx,
		`UPDATE account_members SET role = $3
		  WHERE account_id = $1 AND user_id = $2`, accountID, userID, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
