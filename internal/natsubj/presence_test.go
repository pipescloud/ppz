package natsubj

import (
	"testing"

	"github.com/google/uuid"
)

// ACL Phase 0b — presence moves off the org firehose.
//
// Heartbeats ride <account>.<manifold?>.<handle>.heartbeat today, and
// the daemon picks them up by core-subscribing to <account>.> and
// filtering for a ".heartbeat" suffix client-side
// (internal/daemon/heartbeat_subscriber.go). Live JetStream publishes
// also fan out to core subscribers, so that one subscription sees every
// message published anywhere in the org — regardless of how tightly the
// principal's JetStream permissions are drawn. Read ACLs would be
// bypassable in one line of client code.
//
// Fix: a dedicated prefix, so a single subscription catches every
// heartbeat and the ACL can grant presence separately from everything
// else under the handle.
//
//	<account>.<manifold?>.<handle>.heartbeat  ->  <account>._presence.<manifold?>.<handle>

func TestPresenceSubject_RootManifold(t *testing.T) {
	acct := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	got := PresenceSubject(acct, "", "alice")
	want := acct.String() + "._presence.alice"
	if got != want {
		t.Errorf("PresenceSubject = %q, want %q", got, want)
	}
}

func TestPresenceSubject_NestedManifold(t *testing.T) {
	acct := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	cases := []struct {
		manifold string
		handle   string
		want     string
	}{
		{"ns", "agent-b", acct.String() + "._presence.ns.agent-b"},
		{"team.sub", "agent-c", acct.String() + "._presence.team.sub.agent-c"},
	}
	for _, c := range cases {
		if got := PresenceSubject(acct, c.manifold, c.handle); got != c.want {
			t.Errorf("PresenceSubject(%q, %q) = %q, want %q", c.manifold, c.handle, got, c.want)
		}
	}
}

// The daemon subscribes once, to this prefix, instead of to <account>.>.
func TestPresencePrefix_IsScopedNotOrgWide(t *testing.T) {
	acct := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	got := PresencePrefix(acct)
	want := acct.String() + "._presence.>"
	if got != want {
		t.Errorf("PresencePrefix = %q, want %q", got, want)
	}
	if got == OrgSubscription(acct) {
		t.Fatal("PresencePrefix must not be the org-wide firehose — that is the leak being closed")
	}
}

// ppz who renders agents under BARE handles on every daemon: handleSend
// stamps the publisher's own cache with req.Handle (no manifold), so a
// subscriber that stamped "ns.agent-b" would duplicate the agent and
// break the cross-daemon assertion in
// tests/agent/who-shows-cross-daemon-agents. Parsing must return the
// bare handle at any manifold depth.
func TestParsePresenceSubject_ReturnsBareHandle(t *testing.T) {
	acct := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	cases := []struct {
		subject string
		want    string
	}{
		{PresenceSubject(acct, "", "alice"), "alice"},
		{PresenceSubject(acct, "ns", "agent-b"), "agent-b"},
		{PresenceSubject(acct, "team.sub", "agent-c"), "agent-c"},
	}
	for _, c := range cases {
		got, ok := ParsePresenceSubject(c.subject)
		if !ok {
			t.Errorf("ParsePresenceSubject(%q) not ok", c.subject)
			continue
		}
		if got != c.want {
			t.Errorf("ParsePresenceSubject(%q) = %q, want bare handle %q", c.subject, got, c.want)
		}
	}
}

// Anything that isn't a presence subject must be rejected rather than
// yielding a plausible-looking handle. This is the guard that keeps
// pipe traffic out of the presence cache.
func TestParsePresenceSubject_RejectsPipeSubjects(t *testing.T) {
	acct := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	for _, subj := range []string{
		BuildSubject(acct, "", "alice", "stdout"),
		BuildSubject(acct, "", "alice", "inbox"),
		BuildSubject(acct, "ns", "agent-b", "stdout"),
		BuildSubject(acct, "", "", "room"),
		acct.String() + ".alice.heartbeat", // the pre-0b shape
	} {
		if handle, ok := ParsePresenceSubject(subj); ok {
			t.Errorf("ParsePresenceSubject(%q) accepted as presence (handle=%q)", subj, handle)
		}
	}
}

// The reserved segment cannot collide with a real path: HandleRegex
// forbids a leading underscore, so no source, manifold or pipe can ever
// occupy the _presence slot.
func TestPresenceSegment_CannotBeAHandle(t *testing.T) {
	if err := ValidateHandle(PresenceSegment); err == nil {
		t.Fatalf("%q validates as a handle — presence subjects could collide with pipe subjects", PresenceSegment)
	}
	if err := ValidateUserPipeName(PresenceSegment); err == nil {
		t.Fatalf("%q validates as a user pipe name — presence subjects could collide with pipe subjects", PresenceSegment)
	}
}
