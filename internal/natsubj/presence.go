package natsubj

// Presence subjects — ACL Phase 0b.
//
// Heartbeats used to ride the ordinary pipe grammar as
// <account>.<manifold?>.<handle>.heartbeat, and the daemon collected
// them by core-subscribing to <account>.> and filtering for the
// ".heartbeat" suffix client-side. Live JetStream publishes are also
// delivered to core subscribers, so that one subscription received
// every message published anywhere in the org — every stdout byte of
// every shared terminal, every inbox message between other agents.
//
// No JetStream permission can close that, because the bytes arrive over
// a core subscription that never touches the JS API. Pipe read ACLs
// would have been bypassable in a line of client code.
//
// Presence therefore gets its own subject family:
//
//	<account>._presence.<manifold?>.<handle>
//
// so a daemon subscribes to exactly <account>._presence.> and the ACL
// can grant presence separately from everything else under a handle.
//
// The routing lives here rather than at the call sites because
// provisioning (ensurePipeStream), publishing (send target resolution),
// reading and `ls` all derive their subject and stream name from
// BuildSubject / BuildStreamName. Branching in four places would
// eventually drift; branching in the builders cannot.

import (
	"strings"

	"github.com/google/uuid"
)

// PresenceSegment is the reserved slot separating presence traffic from
// the pipe grammar. It leads with an underscore, which HandleRegex
// forbids, so no source, manifold or pipe can ever occupy it.
const PresenceSegment = "_presence"

// PresencePipe is the pipe name that routes to the presence family.
// Kept as a pipe name (rather than a separate concept) so `ppz ls`
// still lists a source's heartbeat alongside its other pipes.
const PresencePipe = "heartbeat"

// SystemSegment carries control signals the server sends to every
// daemon in an account — currently just "your access changed, re-fetch
// your credential". Leads with an underscore, which HandleRegex forbids,
// so no source, manifold or pipe can occupy it.
const SystemSegment = "_system"

// SystemPrefix is the subscription every daemon holds. Included in every
// compiled credential's sub-allow list: a principal that could not hear
// an invalidation would keep using a credential after its access changed.
func SystemPrefix(accountID uuid.UUID) string {
	return accountID.String() + "." + SystemSegment + ".>"
}

// SystemACLSubject is published when an account's access changes —
// a grant, a revoke, or the enforcement switch being toggled.
//
// NATS evaluates permissions only at connect/credential load, so without
// this a change would not reach a live connection until its credential
// expired. This is what makes "takes effect without a restart" true.
func SystemACLSubject(accountID uuid.UUID) string {
	return accountID.String() + "." + SystemSegment + ".acl"
}

// IsSystemACLSubject reports whether a subject is the ACL invalidation
// signal for any account.
func IsSystemACLSubject(subject string) bool {
	parts := strings.Split(subject, ".")
	return len(parts) == 3 && parts[1] == SystemSegment && parts[2] == "acl"
}

// PresenceSubject builds <account>._presence.<manifold?>.<handle>.
func PresenceSubject(accountID uuid.UUID, manifold, handle string) string {
	var b strings.Builder
	b.WriteString(accountID.String())
	b.WriteByte('.')
	b.WriteString(PresenceSegment)
	if manifold != "" {
		b.WriteByte('.')
		b.WriteString(manifold)
	}
	b.WriteByte('.')
	b.WriteString(handle)
	return b.String()
}

// PresencePrefix is the single subscription a daemon needs to see every
// heartbeat in the account — and nothing else. Replaces the
// OrgSubscription firehose for presence purposes.
func PresencePrefix(accountID uuid.UUID) string {
	return accountID.String() + "." + PresenceSegment + ".>"
}

// ParsePresenceSubject extracts the BARE handle from a presence
// subject, or reports false for anything that isn't one.
//
// Bare is load-bearing: handleSend stamps the publishing daemon's own
// cache with req.Handle (no manifold), so a subscriber that stamped
// "ns.agent-b" would render the same agent twice across daemons and
// break tests/agent/who-shows-cross-daemon-agents.
func ParsePresenceSubject(subject string) (handle string, ok bool) {
	parts := strings.Split(subject, ".")
	// <account>._presence.<handle> is the shortest legal form.
	if len(parts) < 3 || parts[1] != PresenceSegment {
		return "", false
	}
	if _, err := uuid.Parse(parts[0]); err != nil {
		return "", false
	}
	handle = parts[len(parts)-1]
	if handle == "" {
		return "", false
	}
	return handle, true
}

// PresenceStreamName is the JetStream stream backing presence for one
// (manifold, handle). Uses its own "presence_" prefix rather than the
// "pipe_" one so it can never collide with a source legitimately named
// "presence".
func PresenceStreamName(accountID uuid.UUID, manifold, handle string) string {
	hex := strings.ReplaceAll(accountID.String(), "-", "")
	if len(hex) > 8 {
		hex = hex[:8]
	}
	parts := []string{"presence", hex}
	if manifold != "" {
		parts = append(parts, strings.ReplaceAll(manifold, ".", "_"))
	}
	parts = append(parts, handle)
	return strings.Join(parts, "_")
}
