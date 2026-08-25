package daemon

import (
	"sort"
	"testing"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// ACL Phase 0c — one source of truth for the auto-provisioned pipe set.
//
// The ACL default table (docs/ACL.md) is keyed on pipe name: `inbox` is
// write-open, `heartbeat` is read-open, the stdio set is owner-only.
// That table is only sound if "which pipes exist automatically" has a
// single answer.
//
// It currently has two, and they disagree:
//
//   - natsubj.AutoProvisionedPipes lists broadcast/inbox/stdin/stdout/stdctrl
//   - pipesForKind (the one the daemon actually provisions from) returns
//     heartbeat/inbox/stdctrl/stdin/stdout/system for pty sources and
//     inbox for message sources
//
// heartbeat and system were provisioned but never propagated into
// natsubj, so a default derived from the stale set silently granted the
// wrong access on both.

// The set must COVER everything actually provisioned. It is deliberately
// a superset, not an exact mirror: `ppz pipe destroy` glob expansion uses
// it as a skip-list, where retaining a name that no longer exists (e.g.
// the pre-launch `broadcast`) costs nothing, while the ACL default table
// is keyed on it, where a default for an impossible pipe is never
// consulted. Both are safe under a superset; neither is safe under a
// subset.
func TestAutoProvisionedPipes_CoversPipesForKind(t *testing.T) {
	for _, kind := range []cliproto.SourceKind{cliproto.KindPTY, cliproto.KindMessage} {
		for _, p := range pipesForKind(string(kind)) {
			if !natsubj.AutoProvisionedPipes[p] {
				t.Errorf("pipesForKind(%s) provisions %q, missing from natsubj.AutoProvisionedPipes (have %v)",
					kind, p, sortedKeys(natsubj.AutoProvisionedPipes))
			}
		}
	}
}

// broadcast is no longer provisioned by anything, even though the name is
// retained in the skip-list above.
func TestPipesForKind_DoesNotProvisionBroadcast(t *testing.T) {
	for _, kind := range []cliproto.SourceKind{cliproto.KindPTY, cliproto.KindMessage} {
		for _, p := range pipesForKind(string(kind)) {
			if p == "broadcast" {
				t.Errorf("pipesForKind(%s) still provisions broadcast (removed pre-launch, locked decision #16)", kind)
			}
		}
	}
}

// Every pipe the ACL default table names must be a pipe that actually
// gets provisioned — otherwise a row in that table is dead code and the
// pipes it was meant to cover fall through to the owner-only default.
func TestAutoProvisionedPipes_CoversACLDefaultTable(t *testing.T) {
	for _, name := range []string{"inbox", "heartbeat", "stdin", "stdout", "stdctrl", "system"} {
		if !natsubj.AutoProvisionedPipes[name] {
			t.Errorf("%q has an ACL default but is not in natsubj.AutoProvisionedPipes", name)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
