package server

import (
	"testing"

	"github.com/pipescloud/ppz/internal/db"
)

// The org-lifecycle actions carry flat key/value payloads — a kind, a
// role, a state — rather than the retention triple pipe rows use. They
// share one generic renderer instead of getting a bespoke formatter
// each: the shape is genuinely uniform, and a formatter per action is
// how formatRetentionDelta ended up rendering `acl.grant` as
// "ttl=0s, msgs=0, bytes=0" (see formatAuditDelta).

func TestFormatFieldDelta(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after string
		want          string
	}{
		// One side absent — a create has no before, a destroy no after.
		// State the whole thing rather than diffing against nothing.
		{"create states the payload", "", `{"kind":"message"}`, "kind=message"},
		{"destroy states what was lost", `{"kind":"message"}`, "", "kind=message"},

		// Both sides — only what MOVED. Same rule the retention
		// formatter follows: reciting unchanged fields buries the one
		// thing that happened.
		{"kind flip", `{"kind":"message"}`, `{"kind":"pty"}`, "kind message → pty"},
		{"role promotion", `{"role":"member"}`, `{"role":"admin"}`, "role member → admin"},
		{"key revocation", `{"state":"active"}`, `{"state":"revoked"}`, "state active → revoked"},
		{"unchanged fields are omitted",
			`{"kind":"pty","role":"member"}`, `{"kind":"pty","role":"admin"}`, "role member → admin"},

		// Deterministic ordering. Map iteration is randomised in Go, so
		// without a sort the same event renders differently per request
		// and the row can't be quoted or diffed.
		{"multiple fields sort by key", "", `{"prefix":"ab12cd34","label":"ci-bot"}`,
			"label=ci-bot, prefix=ab12cd34"},

		// Never panics, never errors: an audit tab that 500s on one
		// malformed row is worse than one that renders it blank.
		{"both absent", "", "", ""},
		{"malformed json", "{", "}", ""},
		{"non-object payload", "", `["a"]`, ""},
		{"identical payloads render nothing", `{"kind":"pty"}`, `{"kind":"pty"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatFieldDelta([]byte(tc.before), []byte(tc.after))
			if got != tc.want {
				t.Errorf("formatFieldDelta(%q, %q) = %q, want %q", tc.before, tc.after, got, tc.want)
			}
		})
	}
}

// Non-scalar values are rendered as their JSON, not as Go's %v of a
// decoded any — which would print a []any as "[broadcast inbox]" and a
// float64 count as "5e+03".
func TestFormatFieldDelta_ScalarRendering(t *testing.T) {
	for _, tc := range []struct{ after, want string }{
		{`{"count":5000}`, "count=5000"},
		{`{"enforced":true}`, "enforced=true"},
		{`{"label":""}`, "label="},
	} {
		if got := formatFieldDelta(nil, []byte(tc.after)); got != tc.want {
			t.Errorf("formatFieldDelta(nil, %q) = %q, want %q", tc.after, got, tc.want)
		}
	}
}

// The routing bug this guards is real and already documented on
// formatAuditDelta: the default branch is the RETENTION formatter, which
// reads a source payload's missing fields as zero and cheerfully claims
// "ttl=0s, msgs=0, bytes=0" — i.e. the trail asserts that creating a
// source reset some pipe's retention. A misleading audit line is worse
// than none.
func TestFormatAuditDelta_OrgActionsDoNotRenderAsRetention(t *testing.T) {
	for _, action := range []string{
		db.AuditActionSourceCreate, db.AuditActionSourceDestroy, db.AuditActionSourcePromote,
		db.AuditActionKeyCreate, db.AuditActionKeyRevoke,
		db.AuditActionMemberAdd, db.AuditActionMemberRemove, db.AuditActionMemberRole,
		db.AuditActionSvcCreate, db.AuditActionSvcDestroy, db.AuditActionSvcKeyMint,
		db.AuditActionInviteCreate, db.AuditActionInviteRevoke,
		db.AuditActionInviteAccept, db.AuditActionInviteDecline,
	} {
		got := formatAuditDelta(action, nil, []byte(`{"kind":"message"}`))
		if got != "kind=message" {
			t.Errorf("formatAuditDelta(%q, …) = %q, want %q", action, got, "kind=message")
		}
	}
}

// Pipe actions must keep their retention renderer — the generic one
// would print "max_msgs 5000 → 5" instead of "msgs 5000 → 5" and lose
// the ttl's duration formatting.
func TestFormatAuditDelta_PipeActionsStillRenderRetention(t *testing.T) {
	before := `{"ttl_seconds":3600,"max_msgs":5000,"max_bytes":0}`
	after := `{"ttl_seconds":3600,"max_msgs":5,"max_bytes":0}`
	if got := formatAuditDelta(db.AuditActionPipeSet, []byte(before), []byte(after)); got != "msgs 5000 → 5" {
		t.Errorf("pipe.set delta = %q, want %q", got, "msgs 5000 → 5")
	}
}
