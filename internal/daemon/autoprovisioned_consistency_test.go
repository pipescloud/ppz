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
// broadcast was removed pre-launch (locked decision #16) and never
// cleaned out of natsubj; heartbeat and system were added and never
// propagated. A default derived from the stale set would silently grant
// the wrong access on `system` and `heartbeat`.

func TestAutoProvisionedPipes_MatchesPipesForKind(t *testing.T) {
	union := map[string]bool{}
	for _, kind := range []cliproto.SourceKind{cliproto.KindPTY, cliproto.KindMessage} {
		for _, p := range pipesForKind(string(kind)) {
			union[p] = true
		}
	}

	if !equalSets(union, natsubj.AutoProvisionedPipes) {
		t.Errorf("auto-provisioned set disagrees:\n  pipesForKind union      = %v\n  natsubj.AutoProvisioned = %v",
			sortedKeys(union), sortedKeys(natsubj.AutoProvisionedPipes))
	}
}

// broadcast was removed pre-launch. Leaving it in the auto-provisioned
// set means the ACL evaluator would carry a default for a pipe that
// cannot exist.
func TestAutoProvisionedPipes_ExcludesBroadcast(t *testing.T) {
	if natsubj.AutoProvisionedPipes["broadcast"] {
		t.Error("natsubj.AutoProvisionedPipes still lists broadcast (removed pre-launch, locked decision #16)")
	}
	for _, kind := range []cliproto.SourceKind{cliproto.KindPTY, cliproto.KindMessage} {
		for _, p := range pipesForKind(string(kind)) {
			if p == "broadcast" {
				t.Errorf("pipesForKind(%s) still provisions broadcast", kind)
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

func equalSets(a, b map[string]bool) bool {
	count := func(m map[string]bool) int {
		n := 0
		for _, v := range m {
			if v {
				n++
			}
		}
		return n
	}
	if count(a) != count(b) {
		return false
	}
	for k, v := range a {
		if v && !b[k] {
			return false
		}
	}
	return true
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
