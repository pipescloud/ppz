package db

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
)

type APIKey struct {
	ID              uuid.UUID
	AccountID  uuid.UUID
	CreatedByUserID uuid.UUID // user that minted the key (NOT NULL)
	// PrincipalUserID is the identity the key ACTS AS — the subject an
	// ACL grant names. Seeded from CreatedByUserID by migration 0006,
	// but distinct: an ACL Phase 1 service-account key is created_by a
	// human and acts_as the service. Never collapse the two, or the
	// service inherits the human's rights.
	PrincipalUserID uuid.UUID
	KeyHash         string
	KeyPrefix       string
	Label           string
	CreatedAt       time.Time
	// RevokedAt is nil for active keys, set to the revoke time once
	// `POST /api/v1/keys/<id>/revoke` flips it. LookupAPIKey filters
	// out revoked rows; the GUI shows them with strikethrough so the
	// audit trail stays visible.
	RevokedAt *time.Time
}

// Revoked is a small accessor — useful from html/template, which can't
// dereference pointers cleanly.
func (k APIKey) Revoked() bool { return k.RevokedAt != nil }

// GeneratePlaintextKey returns a fresh plaintext API key. Format:
// "ppz_<26 hex chars>" (a UUIDv7 hex without dashes, prefixed). 30 chars total
// makes the 8-char display prefix human-meaningful while leaving plenty of
// entropy.
func GeneratePlaintextKey() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "ppz_" + hex.EncodeToString(raw[:13]), nil // 4 + 26 = 30 chars
}

// KeyPrefix is the first 8 characters AFTER the "ppz_" sentinel. Used for
// display in the GUI and the `ppz status` line. Never use the prefix for auth.
func KeyPrefix(plaintext string) string {
	s := strings.TrimPrefix(plaintext, "ppz_")
	if len(s) < 8 {
		return s
	}
	return s[:8]
}

// Argon2id parameters. Conservative: 1 pass, 64 MiB, 4 lanes, 32 byte tag,
// 16 byte salt. Tune later if profiling shows hot spot.
const (
	a2Time    = 1
	a2Memory  = 64 * 1024
	a2Threads = 4
	a2KeyLen  = 32
	a2SaltLen = 16
)

// HashAPIKey produces a self-describing argon2id hash:
//   $argon2id$v=19$m=65536,t=1,p=4$<base64-salt>$<base64-tag>
func HashAPIKey(plaintext string) (string, error) {
	salt := make([]byte, a2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	tag := argon2.IDKey([]byte(plaintext), salt, a2Time, a2Memory, a2Threads, a2KeyLen)
	enc := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		a2Memory, a2Time, a2Threads, enc(salt), enc(tag)), nil
}

// VerifyAPIKey checks plaintext against a stored hash in constant time.
func VerifyAPIKey(plaintext, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(plaintext), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(want, got) == 1
}

// InsertAPIKey mints a fresh plaintext key, hashes it, and writes the row.
// `createdBy` is the user who minted the key — required (NOT NULL on the
// table) so every key is attributable for `ppz ls` HUMAN.
func InsertAPIKey(ctx context.Context, p *Pool, accountID, createdBy uuid.UUID, label string) (key APIKey, plaintext string, err error) {
	plaintext, err = GeneratePlaintextKey()
	if err != nil {
		return APIKey{}, "", err
	}
	hash, err := HashAPIKey(plaintext)
	if err != nil {
		return APIKey{}, "", err
	}
	key = APIKey{
		ID:              uuid.New(),
		AccountID:       accountID,
		CreatedByUserID: createdBy,
		// Phase 0a: a key acts as whoever minted it. Phase 1 adds an
		// explicit principal argument so a human can mint a key that
		// acts as a service account instead.
		PrincipalUserID: createdBy,
		KeyHash:         hash,
		KeyPrefix:       KeyPrefix(plaintext),
		Label:           label,
		CreatedAt:       time.Now().UTC(),
	}
	_, err = p.Exec(ctx,
		`INSERT INTO api_keys (id, account_id, created_by_user_id, principal_user_id, key_hash, key_prefix, label, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		key.ID, key.AccountID, key.CreatedByUserID, key.PrincipalUserID, key.KeyHash, key.KeyPrefix, key.Label, key.CreatedAt)
	return key, plaintext, err
}

// LookupAPIKey resolves a plaintext key to its row by scanning all keys with
// the matching 8-char prefix and verifying the hash. ErrNotFound when no
// match (including when the matching key has been revoked).
func LookupAPIKey(ctx context.Context, p *Pool, plaintext string) (APIKey, error) {
	prefix := KeyPrefix(plaintext)
	rows, err := p.Query(ctx,
		`SELECT id, account_id, created_by_user_id, principal_user_id, key_hash, key_prefix, label, created_at, revoked_at
		   FROM api_keys WHERE key_prefix = $1 AND revoked_at IS NULL`, prefix)
	if err != nil {
		return APIKey{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.AccountID, &k.CreatedByUserID, &k.PrincipalUserID, &k.KeyHash, &k.KeyPrefix, &k.Label, &k.CreatedAt, &k.RevokedAt); err != nil {
			return APIKey{}, err
		}
		if VerifyAPIKey(plaintext, k.KeyHash) {
			return k, nil
		}
	}
	if err := rows.Err(); err != nil {
		return APIKey{}, err
	}
	return APIKey{}, ErrNotFound
}

// ListAPIKeysForOrg returns every key for the org, including revoked
// ones — the GUI shows revoked keys (with strikethrough) so the audit
// trail stays visible. Sorted active-first by creation time, then
// revoked rows.
func ListAPIKeysForOrg(ctx context.Context, p *Pool, accountID uuid.UUID) ([]APIKey, error) {
	rows, err := p.Query(ctx,
		`SELECT id, account_id, created_by_user_id, principal_user_id, key_hash, key_prefix, label, created_at, revoked_at
		   FROM api_keys
		  WHERE account_id = $1
		  ORDER BY (revoked_at IS NULL) DESC, created_at ASC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.AccountID, &k.CreatedByUserID, &k.PrincipalUserID, &k.KeyHash, &k.KeyPrefix, &k.Label, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey marks the key revoked. Idempotent: revoking an
// already-revoked key is a no-op (returns nil). Returns ErrNotFound if
// no row matches the id.
func RevokeAPIKey(ctx context.Context, p *Pool, id uuid.UUID) error {
	tag, err := p.Exec(ctx,
		`UPDATE api_keys
		    SET revoked_at = NOW()
		  WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Either the row doesn't exist OR it was already revoked.
		// Distinguish via a follow-up SELECT — already-revoked is
		// idempotent (nil), missing is ErrNotFound.
		var exists bool
		if err := p.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM api_keys WHERE id = $1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

// ErrNotFound is returned by lookups when the row does not exist.
var ErrNotFound = errors.New("not found")
