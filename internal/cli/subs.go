package cli

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/daemon"
)

// cmdSubsGroup: ppz subs {ls|add|rm|wait|read}
//
// A curated, per-session subset of pipe subjects — the agent inbox-monitor
// list and the human "I'm in these rooms" set. `wait` and `read` operate
// over this subset instead of `ls --watch`'s firehose across every pipe.
//
//	ppz subs ls                 print the current subscription set (table)
//	ppz subs add <target>...    subscribe to one or more pipes (idempotent)
//	ppz subs rm  [--force] <t>… unsubscribe (idempotent; own inbox guarded)
//	ppz subs wait               block until a subscribed pipe has unread,
//	                            then print ONLY the unread row(s) and exit
//	ppz subs read               read each subscribed pipe that has unread,
//	                            with a `=== <target> ===` separator per pipe
//
// A bare <target> is an uncollared pipe (read-style), stored verbatim; use
// an explicit <handle>.<pipe> for a collared pipe such as an inbox. The
// caller's own inbox is auto-subscribed at source/terminal/agent create.
func cmdSubsGroup(args []string) error {
	if len(args) == 0 {
		printHelp(os.Stderr, "subs")
		os.Exit(2)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "ls":
		return cmdSubsLs(rest)
	case "add":
		return cmdSubsAdd(rest)
	case "rm":
		return cmdSubsRm(rest)
	case "wait":
		return cmdSubsWait(rest)
	case "read":
		return cmdSubsRead(rest)
	case "-h", "--help", "help":
		printHelp(os.Stdout, "subs")
		return nil
	}
	fmt.Fprintf(os.Stderr, "ppz subs: unknown verb %q\n", verb)
	printHelp(os.Stderr, "subs")
	os.Exit(2)
	return nil
}

// cmdSubsLs renders the full subscription set as the same table `ppz ls`
// uses (so users don't learn two formats), enriched with live stats.
func cmdSubsLs(args []string) error {
	fs := flag.NewFlagSet("subs ls", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit one JSON object per subscribed row")
	iso := fs.Bool("iso", false, "render LAST as RFC3339 instead of relative duration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var reply cliproto.ListReply
	if err := daemon.Call(ipcSocket(), cliproto.IPCSubsList,
		cliproto.SubsListRequest{Session: sessionID()}, &reply); err != nil {
		return err
	}
	printSubsList(reply, *asJSON, *iso)
	return nil
}

// printSubsList renders `subs ls`: a tree (pattern parents + matched
// children) for the human table, or flat per-pipe rows with matched_by for
// --json. Distinct from printSubsReply (used by `subs wait`), which keeps the
// flat `ls --watch` shape for the unread-only payload.
func printSubsList(reply cliproto.ListReply, asJSON, iso bool) {
	if asJSON {
		cliproto.PrintSubsListJSON(os.Stdout, reply.Sources, reply.UncollaredPipes)
		return
	}
	cliproto.PrintSubsList(os.Stdout, reply.Sources, reply.UncollaredPipes, reply.Subscriptions, iso)
}

func cmdSubsAdd(args []string) error {
	fs := flag.NewFlagSet("subs add", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	targets := fs.Args()
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ppz subs add <target>...")
		os.Exit(2)
	}
	var reply cliproto.SubsAddReply
	return daemon.Call(ipcSocket(), cliproto.IPCSubsAdd,
		cliproto.SubsAddRequest{Session: sessionID(), Targets: targets}, &reply)
}

func cmdSubsRm(args []string) error {
	fs := flag.NewFlagSet("subs rm", flag.ExitOnError)
	force := fs.Bool("force", false, "remove even your own inbox (override the self-inbox guard)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	targets := fs.Args()
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ppz subs rm [--force] <target>...")
		os.Exit(2)
	}
	var reply cliproto.SubsRemoveReply
	// A guarded own-inbox removal returns E_OWN_INBOX from the daemon, which
	// surfaces here as a non-zero exit — exactly the loud signal we want.
	if err := daemon.Call(ipcSocket(), cliproto.IPCSubsRemove,
		cliproto.SubsRemoveRequest{Session: sessionID(), Targets: targets, Force: *force}, &reply); err != nil {
		return err
	}
	// Per-target feedback so a no-op `rm` is never silent. Removal semantics
	// are unchanged; this just narrates the result.
	for _, o := range reply.Outcomes {
		switch {
		case o.Removed && o.CoveredByPattern != "":
			// The literal sub was removed, but the pipe re-expands under a
			// surviving pattern — say so, or this re-creates the very "I
			// removed it and it came back" confusion the feature kills.
			fmt.Printf("removed: %s (still matched by pattern '%s' — remove the pattern to stop watching it)\n", o.Target, o.CoveredByPattern)
		case o.Removed:
			fmt.Printf("removed: %s\n", o.Target)
		case o.CoveredByPattern != "":
			fmt.Printf("nothing removed: %s is covered by pattern '%s' — remove the pattern to stop watching it\n", o.Target, o.CoveredByPattern)
		default:
			fmt.Printf("nothing removed: no subscription matching '%s'\n", o.Target)
		}
	}
	return nil
}

// cmdSubsWait blocks until a subscribed pipe has unread, then prints only
// the unread row(s) — the token-light hot path for an agent monitor loop.
//
// Like the single-shot `ls --watch` it builds on, a false-positive NATS
// wakeup can occasionally make it return with NO rows (empty output,
// exit 0). Consumers should loop and re-invoke `subs wait` rather than
// treat an empty result as an error or end-of-stream.
func cmdSubsWait(args []string) error {
	fs := flag.NewFlagSet("subs wait", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit unread rows as JSON (same shape as ls --watch --json)")
	iso := fs.Bool("iso", false, "render LAST as RFC3339 instead of relative duration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var reply cliproto.ListReply
	if err := daemon.CallWait(ipcSocket(), cliproto.IPCSubsWait,
		cliproto.SubsWaitRequest{Session: sessionID()}, &reply); err != nil {
		return err
	}
	printSubsReply(reply, *asJSON, *iso)
	return nil
}

// cmdSubsRead reads every subscribed pipe that has unread, in sorted order,
// each block prefixed by a `=== <target> ===` separator so the consumer
// knows which read table belongs to which subscription. The separator is
// omitted under --raw and --json so those stay byte-faithful / parseable.
// Pipes with no unread are skipped. Like `ppz read`, it advances the cursor
// as it goes, and applies the same head-N flood cap PER PIPE (default 10,
// -l N to change, -l 0 to drain everything) — a spammed pipe yields with a
// "(N more unread)" trailer instead of starving the pipes sorted after it.
func cmdSubsRead(args []string) error {
	fs := flag.NewFlagSet("subs read", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON envelopes per message")
	raw := fs.Bool("raw", false, "byte-faithful payloads, no separator")
	tty := fs.Bool("tty", false, "render concatenated payloads through a virtual terminal")
	bare := fs.Bool("bare", false, "legacy payload-only output")
	headLimit := fs.Int("l", defaultReadHeadLimit, "deliver at most the next N oldest unread per pipe (flood cap); 0 = no cap")
	fs.IntVar(headLimit, "limit", defaultReadHeadLimit, "long form of -l")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var reply cliproto.ListReply
	if err := daemon.Call(ipcSocket(), cliproto.IPCSubsList,
		cliproto.SubsListRequest{Session: sessionID()}, &reply); err != nil {
		return err
	}
	// The `=== <target> ===` banner is a human/tty affordance. Suppress it
	// under --raw (which promises byte-faithful output) and --json (which
	// must stay a clean parseable stream); otherwise it breaks both
	// contracts. Default + --tty keep the banner.
	banner := !*raw && !*asJSON
	for _, target := range unreadTargets(reply) {
		if banner {
			fmt.Fprintf(os.Stdout, "=== %s ===\n", target)
		}
		if err := runRead(target, *asJSON, false /* follow */, *tty, *raw, *bare, false /* all */, 0, *headLimit, 0, 0); err != nil {
			return err
		}
	}
	return nil
}

func printSubsReply(reply cliproto.ListReply, asJSON, iso bool) {
	if asJSON {
		cliproto.PrintListJSONWithUncollared(os.Stdout, reply.Sources, reply.UncollaredPipes)
		return
	}
	// long=false: `subs ls` answers "what am I subscribed to", not
	// "how is this pipe configured". Retention would be noise here.
	cliproto.PrintListWithUncollared(os.Stdout, reply.Sources, reply.UncollaredPipes, iso, false)
}

// unreadTargets returns the subscribed pipe targets with unread > 0, sorted
// for deterministic `subs read` ordering. Collared rows render as
// <handle>.<pipe>; uncollared as their pipe path — the same strings
// `ppz read` accepts.
func unreadTargets(reply cliproto.ListReply) []string {
	var targets []string
	for _, s := range reply.Sources {
		for _, p := range s.PipeInfos {
			if p.Unread > 0 {
				targets = append(targets, s.Handle+"."+p.Pipe)
			}
		}
	}
	for _, u := range reply.UncollaredPipes {
		if u.Info.Unread > 0 {
			targets = append(targets, cliproto.FormatPipePath(u.Manifold, "", u.Name))
		}
	}
	sort.Strings(targets)
	return targets
}
