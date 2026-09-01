package db

import "testing"

// The org-lifecycle writers: sources, keys, members, service accounts
// and invites. Until these landed the trail covered only what happened
// INSIDE an org's pipes — it could tell you a pipe's retention changed
// but not that the source it hung off had been destroyed, that a key was
// minted, or that the person who did it had been made an admin an hour
// earlier.
//
// The strings are asserted literally because they are a wire contract:
// every row ever written carries one, and the GUI labels and filters on
// them. Renaming a constant silently reinterprets history.
func TestAuditActions_OrgLifecycleStrings(t *testing.T) {
	for got, want := range map[string]string{
		AuditActionSourceCreate:  "source.create",
		AuditActionSourceDestroy: "source.destroy",
		AuditActionSourcePromote: "source.promote",

		AuditActionKeyCreate: "key.create",
		AuditActionKeyRevoke: "key.revoke",

		AuditActionMemberAdd:    "member.add",
		AuditActionMemberRemove: "member.remove",
		AuditActionMemberRole:   "member.role",

		AuditActionSvcCreate:  "svc.create",
		AuditActionSvcDestroy: "svc.destroy",
		AuditActionSvcKeyMint: "svc.key.mint",

		AuditActionInviteCreate:  "invite.create",
		AuditActionInviteRevoke:  "invite.revoke",
		AuditActionInviteAccept:  "invite.accept",
		AuditActionInviteDecline: "invite.decline",
	} {
		if got != want {
			t.Errorf("audit action = %q, want %q", got, want)
		}
	}
}

// Target types name what Target refers to. A source and a pipe can share
// a string ("chat" is a handle AND a legal uncollared pipe name), so
// without a distinct target type the trail cannot say which one was
// destroyed.
func TestAuditTargets_OrgLifecycleStrings(t *testing.T) {
	for got, want := range map[string]string{
		AuditTargetSource:  "source",
		AuditTargetKey:     "key",
		AuditTargetUser:    "user",
		AuditTargetService: "service",
		AuditTargetInvite:  "invite",
	} {
		if got != want {
			t.Errorf("audit target type = %q, want %q", got, want)
		}
	}
}
