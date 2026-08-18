package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pipescloud/ppz/internal/natsubj"
)

// UserMode = "github" (real OAuth identity) or "internal" (placeholder /
// seeded test user / pre-OAuth-era account).
type UserMode string

const (
	UserModeGithub   UserMode = "github"
	UserModeInternal UserMode = "internal"
	// UserModeService is a service account — an agent's own identity,
	// distinct from the human who spawned it (ACL Phase 1). Key-only
	// auth, scoped to exactly one account.
	UserModeService UserMode = "service"
)

// ServiceNameSeparator scopes a service account's stored username as
// "<org>/<name>". users.username is globally unique, so two orgs could
// not otherwise both hold a "builder-bot". Every surface displays the
// bare name; only storage and lookup see the scoped form.
const ServiceNameSeparator = "/"

type User struct {
	ID        uuid.UUID
	Username  string
	Email     string
	Mode      UserMode
	GitHubID  *int64 // nil for mode=internal users
	AvatarURL string
	CreatedAt time.Time
	// ServiceAccountID is the account a service account belongs to,
	// and nil for humans. The DB CHECK enforces the pairing; the
	// pointer is what makes "human with no owning org" representable.
	ServiceAccountID *uuid.UUID
}

// IsService reports whether this principal is a service account.
func (u User) IsService() bool { return u.Mode == UserModeService }

// ServiceUsername builds the stored, org-scoped username.
func ServiceUsername(orgName, name string) string {
	return orgName + ServiceNameSeparator + name
}

// ParseServiceUsername splits a stored service username back into its
// org and bare name. Human usernames must never parse — otherwise the
// GUI would render "alice" as a service account of some phantom org.
func ParseServiceUsername(username string) (org, name string, ok bool) {
	org, name, found := strings.Cut(username, ServiceNameSeparator)
	if !found || org == "" || name == "" {
		return "", "", false
	}
	if strings.Contains(name, ServiceNameSeparator) {
		return "", "", false
	}
	return org, name, true
}

// ValidateServiceName checks a bare service-account name. It follows
// the handle rules, which forbid the scope separator — a name carrying
// one could otherwise be stored as another org's scoped username.
func ValidateServiceName(name string) (string, error) {
	if err := natsubj.ValidateHandle(name); err != nil {
		return "", fmt.Errorf("service name %q: %w", name, err)
	}
	return name, nil
}

// ErrInvalidUserMode is returned when a caller passes a Mode value
// outside the {github, internal} CHECK constraint.
var ErrInvalidUserMode = errors.New("user mode must be 'github', 'internal' or 'service'")

func InsertUser(ctx context.Context, p *Pool, username, email string, mode UserMode) (User, error) {
	if mode != UserModeGithub && mode != UserModeInternal {
		return User{}, ErrInvalidUserMode
	}
	u := User{
		ID:        uuid.New(),
		Username:  username,
		Email:     email,
		Mode:      mode,
		CreatedAt: time.Now().UTC(),
	}
	_, err := p.Exec(ctx,
		`INSERT INTO users (id, username, email, mode, created_at) VALUES ($1, $2, $3, $4, $5)`,
		u.ID, u.Username, u.Email, string(u.Mode), u.CreatedAt)
	return u, err
}

func GetUser(ctx context.Context, p *Pool, id uuid.UUID) (User, error) {
	var u User
	var mode string
	err := p.QueryRow(ctx,
		`SELECT id, username, email, mode, github_id, COALESCE(avatar_url,''), created_at
		   FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Username, &u.Email, &mode, &u.GitHubID, &u.AvatarURL, &u.CreatedAt)
	u.Mode = UserMode(mode)
	return u, err
}

func GetUserByUsername(ctx context.Context, p *Pool, username string) (User, error) {
	var u User
	var mode string
	err := p.QueryRow(ctx,
		`SELECT id, username, email, mode, created_at FROM users WHERE username = $1`, username).
		Scan(&u.ID, &u.Username, &u.Email, &mode, &u.CreatedAt)
	u.Mode = UserMode(mode)
	return u, err
}

// GetLastSelectedAccount returns the org the user last authorized a CLI
// session into (nil if they never have, or the org was since deleted —
// the FK is ON DELETE SET NULL). Used to default the device-flow org
// dropdown.
func GetLastSelectedAccount(ctx context.Context, p *Pool, userID uuid.UUID) (*uuid.UUID, error) {
	var acct *uuid.UUID
	err := p.QueryRow(ctx,
		`SELECT last_selected_account_id FROM users WHERE id = $1`, userID).Scan(&acct)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return acct, err
}

// SetLastSelectedAccount records the org a user just authorized into so
// the next device-flow verify page defaults its dropdown to it.
func SetLastSelectedAccount(ctx context.Context, p *Pool, userID, accountID uuid.UUID) error {
	_, err := p.Exec(ctx,
		`UPDATE users SET last_selected_account_id = $2 WHERE id = $1`, userID, accountID)
	return err
}

// UsernamesByIDs resolves a set of user IDs to {id → username}. Used by
// the server's list endpoints that need to attribute every source/pipe
// row to a user (HUMAN column). Single round-trip via ANY($1::uuid[]).
// Missing IDs are simply absent from the returned map — callers should
// treat that as "" so a stale ID can't break rendering.
func UsernamesByIDs(ctx context.Context, p *Pool, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// mode is selected so service accounts resolve to their DISPLAY
	// name. Their stored username is org-scoped ("<org>/<name>") because
	// users.username is globally unique; leaking that into `ppz ls`
	// HUMAN / creator columns would show "alpha/builder-bot" where the
	// user typed "builder-bot".
	rows, err := p.Query(ctx,
		`SELECT id, username, mode FROM users WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		var mode string
		if err := rows.Scan(&u.ID, &u.Username, &mode); err != nil {
			return nil, err
		}
		u.Mode = UserMode(mode)
		out[u.ID] = u.DisplayName()
	}
	return out, rows.Err()
}

// UpsertUserByGitHubID inserts a brand new mode=github user, or
// updates the existing row matching the GitHub numeric id. Returns
// the resolved User row plus a bool indicating whether the row was
// freshly created (true) vs already existed (false). Callers use
// the bool to decide whether to auto-create the user's first org.
func UpsertUserByGitHubID(ctx context.Context, p *Pool, githubID int64, username, email, avatarURL string) (User, bool, error) {
	// Existing row?
	var u User
	var mode string
	err := p.QueryRow(ctx,
		`SELECT id, username, email, mode, github_id, avatar_url, created_at
		   FROM users WHERE github_id = $1`, githubID).
		Scan(&u.ID, &u.Username, &u.Email, &mode, &u.GitHubID, &u.AvatarURL, &u.CreatedAt)

	if err == nil {
		u.Mode = UserMode(mode)
		// Refresh email + avatar in case the user changed them on GitHub.
		if _, err := p.Exec(ctx,
			`UPDATE users SET email = $2, avatar_url = $3 WHERE id = $1`,
			u.ID, email, avatarURL); err != nil {
			return User{}, false, err
		}
		u.Email = email
		u.AvatarURL = avatarURL
		return u, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, err
	}

	// Fresh row.
	id := uuid.New()
	now := time.Now().UTC()
	gh := githubID
	u = User{
		ID:        id,
		Username:  username,
		Email:     email,
		Mode:      UserModeGithub,
		GitHubID:  &gh,
		AvatarURL: avatarURL,
		CreatedAt: now,
	}
	if _, err := p.Exec(ctx,
		`INSERT INTO users (id, username, email, mode, github_id, avatar_url, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		u.ID, u.Username, u.Email, string(u.Mode), gh, avatarURL, now); err != nil {
		return User{}, false, err
	}
	return u, true, nil
}