package db

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// ACL Phase 0a — API keys must resolve to a principal.
//
// Today an API key carries `created_by_user_id`, used only to stamp
// attribution on rows the key creates. The authenticated caller itself
// has no identity: requireBearer leaves AuthedCaller.UserID as uuid.Nil
// on the API-key path (internal/server/auth_bearer.go). That means a
// key is functionally "full access to this org, attributed to nobody" —
// and an ACL grant has no subject to name.
//
// PrincipalUserID is the identity the key ACTS AS. It is seeded from
// created_by_user_id by the 0006 migration, but the two are distinct
// concepts and diverge in Phase 1: a key minted for a service account
// is created_by a human and acts_as the service.
//
// Struct-shape tests only, matching the convention in
// apikeys_creator_test.go — DB-state coverage lives in the e2e suite.

func TestAPIKey_HasPrincipalUserIDField(t *testing.T) {
	pid := uuid.New()
	k := APIKey{
		ID:              uuid.New(),
		AccountID:       uuid.New(),
		CreatedByUserID: uuid.New(),
		PrincipalUserID: pid,
		KeyPrefix:       "abcdefgh",
		Label:           "test",
	}
	if k.PrincipalUserID != pid {
		t.Fatalf("PrincipalUserID round-trip mismatch: got %v want %v", k.PrincipalUserID, pid)
	}
}

// PrincipalUserID is uuid.UUID (NOT NULL), not *uuid.UUID. A key that
// acts as nobody is exactly the hole Phase 0a closes, so the type must
// make "no principal" unrepresentable.
func TestAPIKey_PrincipalUserIDIsNotNullable(t *testing.T) {
	f, ok := reflect.TypeOf(APIKey{}).FieldByName("PrincipalUserID")
	if !ok {
		t.Fatal("APIKey has no PrincipalUserID field")
	}
	if got, want := f.Type.String(), "uuid.UUID"; got != want {
		t.Errorf("PrincipalUserID type = %s, want %s (NOT NULL, not a pointer)", got, want)
	}
}

// The creator and the principal are independent. This pins that they
// are two fields rather than one aliased accessor — Phase 1 mints keys
// where a human creator delegates to a service-account principal, and
// collapsing them would silently re-grant the human's rights.
func TestAPIKey_PrincipalIndependentOfCreator(t *testing.T) {
	creator := uuid.New()
	principal := uuid.New()
	k := APIKey{CreatedByUserID: creator, PrincipalUserID: principal}
	if k.CreatedByUserID == k.PrincipalUserID {
		t.Fatal("creator and principal must be independently assignable")
	}
	if k.PrincipalUserID != principal {
		t.Errorf("PrincipalUserID = %v, want %v", k.PrincipalUserID, principal)
	}
	if k.CreatedByUserID != creator {
		t.Errorf("CreatedByUserID = %v, want %v", k.CreatedByUserID, creator)
	}
}
