// Package natsubj builds and parses the ppz subject grammar
//
//	<org_id>.<handle>.<pipe>
//
// where {pipe} ∈ {broadcast, inbox, stdin, stdout}. The optional workspace slot
// between org and handle is reserved by the long-term grammar but unused.
package natsubj

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// HandleRegex per WIRE.md §1: lowercase alnum + hyphens, max 32, no leading
// or trailing hyphen.
var HandleRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// Reserved handles that cannot be used as pipe names. They overlap with the
// channel names so a bare-token target stays unambiguous.
var Reserved = map[string]bool{
	"broadcast": true,
	"inbox":     true,
	"stdin":     true,
	"stdout":    true,
	"stdctrl":   true,
	"system":    true,
	"db":        true,
}

// ValidPipes are the pipe names recognised on the wire — both auto-
// provisioned (broadcast / stdin / stdout / stdctrl) and any future user-
// creatable names. The user-creatable paths add additional validation on
// top (regex, reserved-name check).
//
// stdctrl carries control signals that don't fit on stdin/out — initially
// just JSON resize events, later potentially exit codes, title changes,
// focus events, etc. The web terminal viewer consumes it to keep its
// xterm.js viewport sized to match the source pty.
var ValidPipes = map[string]bool{
	"broadcast": true,
	"inbox":     true,
	"stdin":     true,
	"stdout":    true,
	"stdctrl":   true,
}

// AutoProvisionedPipes covers pipes the server creates automatically for
// sources. Some are also allowed via user pipe creation (`stdin`, `stdout`,
// `stdctrl` for terminal sharing); `inbox` remains reserved from manual
// creation even though it is provisioned automatically.
//
// MUST stay in sync with db.Source.Pipes(), which is the actual
// provisioning truth. The consumer is `ppz pipe destroy` glob expansion,
// which skips these names so a wildcard destroy can't decapitate a live
// source — deleting a terminal's `system` (write-lease control plane) or
// `stdout` leaves the source apparently alive but broken.
//
// `system` and `heartbeat` were missing here until `ppz pipe set` landed.
// The gap was unreachable before: nothing could put those names in the
// user-pipe list the glob walks, because `system` is reserved from
// `pipe create`. `pipe set` materialises a pipes row for auto-pipes on
// first retention override, which does surface them there.
//
// `broadcast` is retained though Phase 1 removed it (locked decision
// #16) — old rows may still name it, and skipping a name that no longer
// exists costs nothing.
//
// ACL Phase 0c: the ACL default table (docs/ACL.md) is keyed on these
// names too, and wants the same superset. A default for a pipe that
// cannot exist is never consulted, whereas a set MISSING a provisioned
// name silently grants the wrong access — which is what `system` and
// `heartbeat` were doing before they were added here.
var AutoProvisionedPipes = map[string]bool{
	"broadcast": true,
	"inbox":     true,
	"stdin":     true,
	"stdout":    true,
	"stdctrl":   true,
	"system":    true,
	"heartbeat": true,
}

// ReservedPipeNames are names blocked from user pipe creation. Reserved
// for future system use (e.g. an inbox routing scheme, db-backed pipes).
var ReservedPipeNames = map[string]bool{
	"system": true,
	"db":     true,
	"inbox":  true,
	// ACL Phase 0b: `heartbeat` routes to the presence subject family
	// (presence.go). A user-created pipe of that name would collide
	// with a source's presence stream.
	"heartbeat": true,
}

// ValidChannels is a deprecated alias kept during the Phase A rename for any
// caller still phrasing the check as "channel". Will be removed in Phase B.
var ValidChannels = ValidPipes

func ValidateHandle(h string) error {
	if !HandleRegex.MatchString(h) {
		return errors.New("invalid handle")
	}
	if Reserved[h] {
		return errors.New("reserved handle")
	}
	return nil
}

// ValidatePipe checks a pipe name's *syntax* — regex only, no auto/reserved
// restrictions. Used by `read` / `send` / `broadcast` targets, where any
// existing pipe is a legitimate destination (auto-provisioned or user-
// created). The "does this stream actually exist" check is deferred to
// the publish/read path against JetStream.
//
// User pipe creation goes through ValidateUserPipeName, which adds the
// auto/reserved restrictions on top of the regex.
func ValidatePipe(name string) error {
	if !HandleRegex.MatchString(name) {
		return errors.New("invalid pipe")
	}
	return nil
}

// ValidateUserPipeName validates a name passed to `ppz pipe create`.
// Same handle regex (lowercase alnum + hyphens, max 32, no leading/trailing
// hyphen). Reserved names are rejected. Auto-provisioned names (broadcast,
// stdin, stdout) ARE allowed: any source can have arbitrary pipes added
// to it, and the pipe-create path is idempotent against an already-existing
// stream.
func ValidateUserPipeName(name string) error {
	if !HandleRegex.MatchString(name) {
		return errors.New("invalid pipe name")
	}
	if ReservedPipeNames[name] {
		return errors.New("name is reserved")
	}
	return nil
}

// ValidateChannel is the Phase A backward-compat alias for ValidatePipe.
// Removed in Phase B.
func ValidateChannel(c string) error { return ValidatePipe(c) }

// Subject builds <account>.<handle>.<pipe>. Pre-Phase-1.5 three-role
// builder; equivalent to BuildSubject(accountID, "", handle, pipe).
// Retained for callers that haven't been threaded through manifold yet.
func Subject(accountID uuid.UUID, handle, pipe string) string {
	return fmt.Sprintf("%s.%s.%s", accountID.String(), handle, pipe)
}

// BuildSubject emits the four-role subject form per locked decision #18:
//
//	<account>.<manifold?>.<source?>.<pipe>
//
// where manifold is 0+ dot-separated segments ('' = root) and source
// ("collar") is 0 or 1 segment ('' = uncollared). Wire-level the
// manifold-only and source-only shapes are indistinguishable
// (acct.X.pipe could be either) — that's by design; disambiguation
// happens by DB row at create time. The builder just emits the
// canonical dotted form.
func BuildSubject(accountID uuid.UUID, manifold, source, pipe string) string {
	// ACL Phase 0b: presence gets its own subject family so a daemon
	// can subscribe to heartbeats without subscribing to every message
	// in the org. Routed here so provisioning, publishing, reading and
	// `ls` all agree — see presence.go.
	if pipe == PresencePipe {
		return PresenceSubject(accountID, manifold, source)
	}
	var b strings.Builder
	b.WriteString(accountID.String())
	if manifold != "" {
		b.WriteByte('.')
		b.WriteString(manifold)
	}
	if source != "" {
		b.WriteByte('.')
		b.WriteString(source)
	}
	b.WriteByte('.')
	b.WriteString(pipe)
	return b.String()
}

// StreamPrefixes lists every prefix ppz uses for its own JetStream
// streams. IsPPZStream is the single check callers should use — the
// admin wipe originally hard-coded {source_, pipe_}, so when ACL Phase
// 0b added the presence_ family those streams survived every reset and
// heartbeats accumulated across e2e scenarios.
var StreamPrefixes = []string{"source_", "pipe_", "presence_"}

// IsPPZStream reports whether a JetStream stream name belongs to ppz.
// Anything else (internal $JS machinery, streams a user made directly)
// is left alone.
func IsPPZStream(name string) bool {
	for _, p := range StreamPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// StreamName produces the JetStream stream name per WIRE.md §2:
//
//	source_<orgshort>_<handle>_<pipe>
//
// where orgshort is the first 8 hex chars of the org UUID, hyphens stripped.
// Pre-Phase-1.5 three-role form; for the four-role form use BuildStreamName.
func StreamName(accountID uuid.UUID, handle, pipe string) string {
	hex := strings.ReplaceAll(accountID.String(), "-", "")
	if len(hex) > 8 {
		hex = hex[:8]
	}
	return "source_" + hex + "_" + handle + "_" + pipe
}

// BuildStreamName produces a JetStream stream name for the four-role pipe
// shape. NATS stream names can't contain dots, so manifold dots are
// replaced with underscores. Empty manifold/source slots are omitted
// entirely — handle regex forbids underscores in segments so there's no
// ambiguity.
//
//	pipe_<orgshort>[_<manifold-underscored>][_<source>]_<name>
func BuildStreamName(accountID uuid.UUID, manifold, source, name string) string {
	// Presence streams carry their own prefix — see BuildSubject.
	if name == PresencePipe {
		return PresenceStreamName(accountID, manifold, source)
	}
	hex := strings.ReplaceAll(accountID.String(), "-", "")
	if len(hex) > 8 {
		hex = hex[:8]
	}
	parts := []string{"pipe", hex}
	if manifold != "" {
		parts = append(parts, strings.ReplaceAll(manifold, ".", "_"))
	}
	if source != "" {
		parts = append(parts, source)
	}
	parts = append(parts, name)
	return strings.Join(parts, "_")
}

// OrgSubscription is the wildcard subscription used by the server-side
// subscriber and by the daemon's NATS user JWT permission set.
func OrgSubscription(accountID uuid.UUID) string {
	return accountID.String() + ".>"
}
