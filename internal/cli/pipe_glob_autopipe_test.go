package cli

import (
	"testing"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// `ppz pipe destroy '*'` must never sweep away an auto-provisioned pipe:
// destroying a terminal's `system` (write-lease control plane) or
// `stdout` breaks the source while leaving it apparently alive.
//
// resolvePipeGlob skips them via natsubj.AutoProvisionedPipes, but that
// set was missing `system` and `heartbeat` — two names Source.Pipes()
// genuinely auto-provisions for pty sources. It went unnoticed because
// nothing could put those names in the user-pipe list: `system` is
// reserved from `pipe create`, so no row could exist for it.
//
// `ppz pipe set` changes that. It materialises a pipes row for auto-pipes
// on first override — which is the whole point, since those are the pipes
// whose default caps bite first — and that row then surfaces in the
// user-pipe list the glob walks. So a user who retunes a terminal's
// system pipe and later runs `pipe destroy '*'` would destroy it.
func TestResolvePipeGlob_SkipsEveryAutoProvisionedPipe(t *testing.T) {
	// The full pty auto-pipe set, as Source.Pipes() defines it.
	autoPipes := []string{"stdin", "stdout", "stdctrl", "system", "inbox", "heartbeat"}

	infos := make([]cliproto.PipeInfo, 0, len(autoPipes)+1)
	for _, p := range autoPipes {
		infos = append(infos, cliproto.PipeInfo{Pipe: p})
	}
	// One genuine user pipe, so we can tell "skipped everything" from
	// "matched nothing".
	infos = append(infos, cliproto.PipeInfo{Pipe: "archive"})

	sources := []cliproto.Source{{Handle: "term", Kind: "pty", PipeInfos: infos}}

	collared, _, err := resolvePipeGlob("*", sources, nil)
	if err != nil {
		t.Fatalf("resolvePipeGlob: %v", err)
	}

	var got []string
	for _, req := range collared {
		got = append(got, req.Name)
	}
	if len(got) != 1 || got[0] != "archive" {
		t.Errorf("`pipe destroy '*'` resolved to %v, want [archive] only — every auto-provisioned pipe must be skipped", got)
	}
}
