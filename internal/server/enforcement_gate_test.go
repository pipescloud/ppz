package server

import (
	"testing"

	"github.com/pipescloud/ppz/internal/acl"
)

// ACL Phase 3 — the per-org opt-in gate.
//
// `accounts.acl_enforced` defaults false for every org, existing and
// new. Enforcement points read it; nothing else about an org changes on
// deploy.
//
// These cover the pure decision — given the flag and the caller's
// effective access, what permissions go into the credential — so the
// upgrade-safety property is testable without a Postgres or a NATS.

const gateAcct = "22222222-2222-2222-2222-222222222222"

func gateAccess() []acl.Access {
	return []acl.Access{
		{Pipe: acl.PipeRef{Path: "alice.stdout", Stream: "pipe_2_alice_stdout"}, Perm: acl.Read},
	}
}

func containsPerm(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// THE upgrade-safety property. Every org ships with acl_enforced=false,
// so every org must keep getting the wide-open credential it has today.
// If this regresses, every existing deployment breaks the moment the
// release lands — before anyone has opted into anything.
func TestEnforcement_OffMintsUnrestrictedCredential(t *testing.T) {
	perms := credentialPermissions(false, gateAcct, gateAccess())

	if !containsPerm(perms.PubAllow, ">") {
		t.Errorf("an org with enforcement off must still publish anywhere: %v", perms.PubAllow)
	}
	if !containsPerm(perms.SubAllow, ">") {
		t.Errorf("an org with enforcement off must still subscribe anywhere: %v", perms.SubAllow)
	}
	if len(perms.PubDeny) != 0 || len(perms.SubDeny) != 0 {
		t.Errorf("enforcement off must deny nothing: pubDeny=%v subDeny=%v", perms.PubDeny, perms.SubDeny)
	}
}

func TestEnforcement_OnMintsCompiledCredential(t *testing.T) {
	perms := credentialPermissions(true, gateAcct, gateAccess())

	if containsPerm(perms.PubAllow, ">") {
		t.Fatalf("enforcement on must not hand out the wide-open credential: %v", perms.PubAllow)
	}
	if !containsPerm(perms.PubAllow, "$JS.API.STREAM.INFO.pipe_2_alice_stdout") {
		t.Errorf("enforcement on must compile the read grant: %v", perms.PubAllow)
	}
	if !containsPerm(perms.PubDeny, "$JS.API.STREAM.LIST") {
		t.Errorf("enforcement on must deny stream enumeration: %v", perms.PubDeny)
	}
}

// The flag is the only difference. Same access, two answers — pinned so
// a future refactor can't quietly make "off" mean "compiled with
// everything", which would look fine in tests and break on an org whose
// grants are incomplete.
func TestEnforcement_FlagIsTheOnlyDifference(t *testing.T) {
	off := credentialPermissions(false, gateAcct, gateAccess())
	on := credentialPermissions(true, gateAcct, gateAccess())

	if len(off.PubAllow) == len(on.PubAllow) && len(off.PubDeny) == len(on.PubDeny) {
		t.Error("enforcement on and off produced indistinguishable credentials")
	}
}

// A principal with no grants under enforcement still needs a usable
// credential — its own inbox, presence, and the invalidation channel —
// or `ppz who` and credential refresh break for everyone without
// grants, which on a freshly-enabled org is most people.
func TestEnforcement_OnStillAllowsPresenceAndRefresh(t *testing.T) {
	perms := credentialPermissions(true, gateAcct, nil)

	for _, want := range []string{"_INBOX.>", gateAcct + "._presence.>", gateAcct + "._system.>"} {
		if !containsPerm(perms.SubAllow, want) {
			t.Errorf("enforcement on must keep %q subscribable: %v", want, perms.SubAllow)
		}
	}
}
