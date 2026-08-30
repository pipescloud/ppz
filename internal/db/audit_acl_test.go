package db

import (
	"strings"
	"testing"
)

// ACL changes belong in the audit trail — PR #191 landed the trail after
// docs/ACL.md had already listed "audit log of grant changes" as a
// non-goal, and that non-goal only existed because no trail existed.
//
// An access-control change is arguably the thing most worth auditing in
// the product: it is the one edit whose whole purpose is to alter who
// can see what.

func TestAuditActions_ACLAreDottedNounVerb(t *testing.T) {
	for name, got := range map[string]string{
		"grant":   AuditActionACLGrant,
		"revoke":  AuditActionACLRevoke,
		"enforce": AuditActionACLEnforce,
	} {
		if got == "" {
			t.Errorf("%s: action constant is empty", name)
			continue
		}
		// The strings are persisted on every row and the GUI filters on
		// them, so the shape is a contract, not a label.
		if !strings.HasPrefix(got, "acl.") || strings.Count(got, ".") != 1 {
			t.Errorf("%s: %q should read <noun>.<verb> under the acl noun", name, got)
		}
	}
}

// Grant and revoke name a pipe selector; the enforcement switch names
// the org itself, so it needs its own target type rather than being
// filed under a pipe that does not exist.
func TestAuditTarget_OrgIsDistinctFromPipe(t *testing.T) {
	if AuditTargetOrg == "" {
		t.Fatal("AuditTargetOrg is empty")
	}
	if AuditTargetOrg == AuditTargetPipe {
		t.Errorf("org and pipe target types must differ; both are %q", AuditTargetOrg)
	}
}
