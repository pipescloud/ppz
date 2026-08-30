package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/daemon"
)

// cmdLs: ppz ls [--json | --iso] [--watch] [PATTERN...]
//
// Default: aligned table — PIPE / TOTAL / UNREAD / LAST / PAYLOAD with
// relative time in the LAST column. Tuned for human + agent scanability.
//
//	--json    one JSONL row per (handle, pipe), full untruncated payload,
//	          ISO last_at. The agent-friendly path.
//	--iso     keep the table layout but render LAST as RFC3339 timestamps
//	          instead of relative durations. For when you want sortable /
//	          diffable timestamps without dropping into JSON.
//	--watch   block until the calling session has at least one unread
//	          message on a matching pipe, then print the snapshot and
//	          exit. Level-triggered: if unread > 0 already, returns
//	          immediately.
//	PATTERN…  optional glob(s) matched against the full `<handle>.<pipe>`
//	          target (uncollared: the bare/dotted pipe path); multiple
//	          OR-combine; no pattern means every pipe. Works for both the
//	          plain snapshot and --watch — `ppz ls clancy%` filters the
//	          table the same way `ls clancy*` would on the filesystem.
//
//	          Full-name matching: `ls room` lists only the uncollared
//	          `room`; use `ls '*.room'` for a collared `<handle>.room`,
//	          and `ls 'alice.*'` to list a whole handle's pipes. A
//	          fully-specified literal that matches no pipe warns and
//	          returns nothing (`ls Mus` vs `ls Mus*`); a glob is the
//	          speculative form and never warns.
//
//	          Wildcards: `*` (standard glob, must be quoted in zsh:
//	          `'agent-*'`) or `%` (SQL-LIKE-style alias, passes through
//	          unquoted: `agent-%`). Both work the same.
//
// --json and --iso are mutually exclusive — JSON mode always emits ISO
// timestamps, so --iso would be a no-op tag.
func cmdLs(args []string) error {
	if wantsHelp(args) {
		printHelp(os.Stdout, "ls")
		return nil
	}
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit one JSON object per row (agent-friendly, full payload)")
	iso := fs.Bool("iso", false, "render last-message column as RFC3339 timestamp instead of relative duration")
	watch := fs.Bool("watch", false, "block until matching pipes have unread messages, print, then exit")
	// `-l` after `ls -l`: same rows, extra detail columns. Registered
	// twice because Go's flag package has no alias mechanism; it treats
	// one and two dashes identically, so this covers -l/--l/-long/--long.
	long := fs.Bool("l", false, "long form: add TTL / MAXMSGS / MAXBYTES retention columns")
	fs.BoolVar(long, "long", false, "alias for -l")

	// Go's flag package stops at the first non-flag argument, so `ls PATTERN
	// --json` would swallow --json into the pattern list. ls's flags are all
	// booleans (none take a value), so we can pre-separate flags from
	// positional patterns to support any ordering. Mirrors cmdCommand.
	var flagArgs, patterns []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			patterns = append(patterns, a)
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if *asJSON && *iso {
		os.Stderr.WriteString("ppz ls: --json and --iso are mutually exclusive\n")
		os.Exit(2)
	}

	// Snapshot first. It's the non-watch output, and for both modes the
	// basis for the literal-miss warning: a fully-specified literal target
	// that matches no current pipe is almost always a typo or stale
	// handle-watch muscle-memory, so we warn and steer to the glob form
	// (`ls Mus` vs `ls Mus*`). For --watch this preflight lets us warn
	// before the watch blocks — a warning, not an error, so pre-arming a
	// watch on a not-yet-existent pipe still works.
	var snap cliproto.ListReply
	if err := daemon.Call(ipcSocket(), cliproto.IPCList,
		cliproto.ListRequest{Session: sessionID(), Patterns: patterns, Long: *long}, &snap); err != nil {
		return err
	}
	warnLiteralMisses(patterns, snap)

	reply := snap
	if *watch {
		reply = cliproto.ListReply{}
		req := cliproto.ListWatchRequest{Session: sessionID(), Patterns: patterns, Long: *long}
		if err := daemon.CallWait(ipcSocket(), cliproto.IPCListWatch, req, &reply); err != nil {
			return err
		}
	}

	if *asJSON {
		cliproto.PrintListJSONWithUncollared(os.Stdout, reply.Sources, reply.UncollaredPipes)
	} else {
		cliproto.PrintListWithUncollared(os.Stdout, reply.Sources, reply.UncollaredPipes, *iso, *long)
	}
	maybeNotifyUpdate()
	return nil
}

// warnLiteralMisses emits a stderr warning for each fully-specified literal
// pattern (no glob metacharacters) that matched no row in the snapshot,
// steering the user to the glob form. Glob patterns are speculative — they
// may legitimately match nothing now — and never warn.
func warnLiteralMisses(patterns []string, reply cliproto.ListReply) {
	if len(patterns) == 0 {
		return
	}
	matched := map[string]bool{}
	for _, s := range reply.Sources {
		for _, p := range s.PipeInfos {
			matched[s.Handle+"."+p.Pipe] = true
		}
	}
	for _, u := range reply.UncollaredPipes {
		matched[cliproto.FormatPipePath(u.Manifold, "", u.Name)] = true
	}
	for _, raw := range patterns {
		if strings.ContainsAny(raw, "*?[%") {
			continue // glob: speculative, may match nothing without warning
		}
		if !matched[raw] {
			fmt.Fprintf(os.Stderr,
				"ppz ls: no pipe matches %q — use a glob like %q to match speculatively\n",
				raw, raw+"%")
		}
	}
}
