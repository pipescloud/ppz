package db

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// ACL Phase 1 — service accounts.
//
// An agent needs its own identity, distinct from the human who spawned
// it: a real principal that holds grants, owns handles, and is
// attributed on `ppz who` and on every message it publishes. It
// differs from a human only in how it authenticates (key only, no
// OAuth) and in being scoped to one account.
//
// Struct-shape and pure-function tests only — DB-state coverage lives
// in the e2e suite, matching the convention in apikeys_creator_test.go.

func TestUser_HasServiceAccountID(t *testing.T) {
	org := uuid.New()
	u := User{Username: "acme/builder-bot", Mode: "service", ServiceAccountID: &org}
	if u.ServiceAccountID == nil || *u.ServiceAccountID != org {
		t.Fatalf("ServiceAccountID round-trip mismatch: got %v want %v", u.ServiceAccountID, org)
	}
}

// Nullable: humans have no owning account, services must. The CHECK
// constraint enforces the pairing in the DB; the pointer type is what
// makes "human with no owning org" representable in Go.
func TestUser_ServiceAccountIDIsNullable(t *testing.T) {
	f, ok := reflect.TypeOf(User{}).FieldByName("ServiceAccountID")
	if !ok {
		t.Fatal("User has no ServiceAccountID field")
	}
	if got, want := f.Type.String(), "*uuid.UUID"; got != want {
		t.Errorf("ServiceAccountID type = %s, want %s (NULL for humans)", got, want)
	}
}

func TestUser_IsService(t *testing.T) {
	org := uuid.New()
	cases := []struct {
		name string
		user User
		want bool
	}{
		{"service", User{Mode: "service", ServiceAccountID: &org}, true},
		{"github human", User{Mode: "github"}, false},
		{"internal human", User{Mode: "internal"}, false},
	}
	for _, c := range cases {
		if got := c.user.IsService(); got != c.want {
			t.Errorf("%s: IsService() = %v, want %v", c.name, got, c.want)
		}
	}
}

// users.username is globally unique, so two orgs cannot both hold a
// bare "builder-bot". Service rows store "<org>/<name>" and display the
// bare name in-org. Contained wart, but it avoids relaxing a constraint
// every other lookup depends on.
func TestServiceUsername_ScopedPerOrg(t *testing.T) {
	acme := ServiceUsername("acme", "builder-bot")
	globex := ServiceUsername("globex", "builder-bot")
	if acme == globex {
		t.Fatalf("same bare name in two orgs collided: %q", acme)
	}
	if acme != "acme/builder-bot" {
		t.Errorf("ServiceUsername = %q, want %q", acme, "acme/builder-bot")
	}
}

func TestParseServiceUsername_RoundTrip(t *testing.T) {
	org, name, ok := ParseServiceUsername(ServiceUsername("acme", "builder-bot"))
	if !ok {
		t.Fatal("round-trip failed to parse")
	}
	if org != "acme" || name != "builder-bot" {
		t.Errorf("parsed (%q, %q), want (acme, builder-bot)", org, name)
	}
}

// A human username must never parse as a service one — otherwise the
// GUI would render "alice" as a service account of some phantom org.
func TestParseServiceUsername_RejectsHumanUsernames(t *testing.T) {
	// Positive control: a parser that rejects everything would satisfy
	// the loop below without ever parsing a real service username.
	if _, _, ok := ParseServiceUsername("acme/builder-bot"); !ok {
		t.Fatal("control: a scoped service username must parse")
	}
	for _, u := range []string{"alice", "foo", "gh-test-user", ""} {
		if _, _, ok := ParseServiceUsername(u); ok {
			t.Errorf("%q parsed as a service username", u)
		}
	}
}

// The bare name is what a user types at `ppz pipe acl grant … builder-bot`
// and what every surface displays. Service names follow the handle
// rules so they can't smuggle a slash and forge another org's scope.
func TestServiceUsername_RejectsSeparatorInName(t *testing.T) {
	if _, err := ValidateServiceName("evil/name"); err == nil {
		t.Error("a service name containing the scope separator must be rejected")
	}
	if _, err := ValidateServiceName("builder-bot"); err != nil {
		t.Errorf("valid service name rejected: %v", err)
	}
}

// Phase 1 lets a human mint a key that acts as a service account. The
// creator and the principal diverge here for the first time.
func TestInsertAPIKeyAs_SignatureAcceptsPrincipal(t *testing.T) {
	// Compile-time only: pins the signature without a live pool.
	_ = func(pool *Pool, accountID, createdBy, principal uuid.UUID) {
		_, _, _ = InsertAPIKeyAs(nil, pool, accountID, createdBy, principal, "label")
	}
}
