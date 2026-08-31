package server

import (
	"testing"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
)

// ACL Phase 0a — the authenticated caller always has an identity.
//
// resolveBearer currently returns AuthedCaller{APIKey: &key} on the
// API-key path, leaving UserID as uuid.Nil (auth_bearer.go). Every
// downstream consumer that wants a user therefore has to reject API
// keys outright — see the `caller.UserID == uuid.Nil` guards all over
// handlers_invites.go, which answer "OAuth token required".
//
// ACLs name a principal, so this hole has to close first: both auth
// surfaces must produce the same shape.
//
// These tests target the pure constructors that resolveBearer will
// delegate to, so the behaviour is unit-testable without a Postgres.
// The DB lookups themselves stay covered by the e2e suite, matching
// the note at the top of auth_bearer_test.go.

func TestCallerFromAPIKey_PopulatesUserID(t *testing.T) {
	principal := uuid.New()
	key := db.APIKey{
		ID:              uuid.New(),
		AccountID:       uuid.New(),
		CreatedByUserID: principal,
		PrincipalUserID: principal,
	}

	caller := callerFromAPIKey(key)

	if caller.UserID == uuid.Nil {
		t.Fatal("UserID is uuid.Nil — an API key must resolve to a principal")
	}
	if caller.UserID != principal {
		t.Errorf("UserID = %v, want %v", caller.UserID, principal)
	}
	if caller.APIKey == nil {
		t.Error("APIKey must stay populated so requireAPIKey keeps working")
	}
}

// The caller acts as the key's principal, NOT as whoever minted it.
// With Phase 1's service-account keys a human mints a key that acts as
// a bot; reading the creator here would hand the bot the human's
// rights, which is the escalation this whole plan exists to prevent.
func TestCallerFromAPIKey_UsesPrincipalNotCreator(t *testing.T) {
	creator := uuid.New()
	principal := uuid.New()
	key := db.APIKey{
		ID:              uuid.New(),
		AccountID:       uuid.New(),
		CreatedByUserID: creator,
		PrincipalUserID: principal,
	}

	caller := callerFromAPIKey(key)

	if caller.UserID == creator {
		t.Fatal("UserID follows the key's creator — it must follow the principal")
	}
	if caller.UserID != principal {
		t.Errorf("UserID = %v, want principal %v", caller.UserID, principal)
	}
}

// Regression: the OAuth path already populates UserID and must keep
// doing so once both paths share a shape.
func TestCallerFromOAuthToken_PopulatesUserID(t *testing.T) {
	tokenID := uuid.New()
	userID := uuid.New()

	caller := callerFromOAuthToken(db.OAuthToken{ID: tokenID, UserID: userID})

	if caller.UserID != userID {
		t.Errorf("UserID = %v, want %v", caller.UserID, userID)
	}
	if caller.TokenID == nil {
		t.Fatal("TokenID must stay populated on the OAuth path")
	}
	if *caller.TokenID != tokenID {
		t.Errorf("TokenID = %v, want %v", *caller.TokenID, tokenID)
	}
	if caller.APIKey != nil {
		t.Error("APIKey must be nil on the OAuth path")
	}
}

// Both surfaces answer the same question the same way. Phase 2's
// evaluator reads Principal() and must not care which credential
// carried the request.
func TestAuthedCaller_PrincipalIsUniformAcrossSurfaces(t *testing.T) {
	userID := uuid.New()

	viaKey := callerFromAPIKey(db.APIKey{PrincipalUserID: userID})
	viaToken := callerFromOAuthToken(db.OAuthToken{ID: uuid.New(), UserID: userID})

	if viaKey.Principal() != viaToken.Principal() {
		t.Fatalf("principal differs by auth surface: key=%v token=%v",
			viaKey.Principal(), viaToken.Principal())
	}
	if viaKey.Principal() != userID {
		t.Errorf("Principal() = %v, want %v", viaKey.Principal(), userID)
	}
}

// An unauthenticated context has no principal. Guards against
// Principal() inventing a zero-value identity that then matches an
// ACL row keyed on uuid.Nil.
func TestAuthedCaller_ZeroValueHasNoPrincipal(t *testing.T) {
	var caller AuthedCaller
	if caller.Principal() != uuid.Nil {
		t.Errorf("zero-value Principal() = %v, want uuid.Nil", caller.Principal())
	}
}
