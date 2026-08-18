package db

import (
	"testing"

	"github.com/pipescloud/ppz/internal/natsubj"
)

// DRIFT GUARD between the two independent lists of auto-provisioned pipe
// names:
//
//	db.Source.Pipes()             — what the server actually provisions
//	natsubj.AutoProvisionedPipes  — what callers use to recognise one
//
// The bug this exists for: AutoProvisionedPipes was missing `system` and
// `heartbeat`, both of which Source.Pipes() provisions for pty sources.
// That made `ppz pipe destroy '*'` willing to destroy a live terminal's
// write-lease pipe. The CLI-side test for that pins its own copy of the
// name list, so it catches today's bug but not the next divergence —
// adding a pipe to Source.Pipes() and forgetting the map would sail past
// it. This compares the two sources of truth directly, so the drift
// itself is what fails.
func TestSourcePipes_AreAllKnownAsAutoProvisioned(t *testing.T) {
	for _, kind := range []SourceKind{SourceKindMessage, SourceKindPTY} {
		for _, name := range (Source{Kind: kind}).Pipes() {
			if !natsubj.AutoProvisionedPipes[name] {
				t.Errorf("Source.Pipes() provisions %q for kind %q, but natsubj.AutoProvisionedPipes doesn't list it — "+
					"anything that recognises auto-pipes by that map (e.g. the `pipe destroy '*'` skip) will treat it as a user pipe",
					name, kind)
			}
		}
	}
}
