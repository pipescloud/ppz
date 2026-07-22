package cli

// Help content + rendering for `ppz`.
//
// Two surfaces share one renderer (wrapUsageText, root.go) so width
// handling lives in exactly one place:
//
//   - Top-level `ppz help` / `ppz --help`: a scannable, grouped list with
//     ONE short summary line per command (renderTopLevel + topLevelGroups).
//   - Per-command / per-topic detail: `ppz <verb> --help`, `ppz help <verb>`,
//     and `ppz help <topic>` all look up a body in helpTopics.
//
// The detailed prose that used to be crammed into the top-level block now
// lives in helpTopics, keyed by verb path ("read", "source destroy") and by
// named topic ("acks", "sessions", "globs").
//
// CONVENTION for helpTopics bodies: each prose paragraph is a SINGLE source
// line (no hand-wrapping) so wrapUsageText reflows it to the caller's terminal
// width. Indented flag/example tables use a 2+ space gap after the flag so the
// renderer's description-column detection wraps their descriptions too.

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// helpRow is one command line in the grouped top-level help: a short
// signature (e.g. "ppz send TGT PAYLOAD") and a one-clause summary.
type helpRow struct {
	sig     string
	summary string
}

// helpGroup is a titled cluster of related commands in the top-level help.
type helpGroup struct {
	title string
	rows  []helpRow
}

// topLevelGroups is the source of truth for the grouped `ppz help` layout.
// Keep every leading verb in sync with completion.go's topLevelVerbs /
// subverbs — TestHelpGroups_CoverTopLevelVerbs enforces it.
var topLevelGroups = []helpGroup{
	{"GETTING STARTED", []helpRow{
		{"ppz login URL", "log in to a server (e.g. ppz login pipescloud.io)"},
	}},
	{"MESSAGING", []helpRow{
		{"ppz status", "daemon state, current handle, last token refresh"},
		{"ppz ls [--watch] [PATTERN]", "list handles × pipes; --watch blocks until unread"},
		{"ppz read TGT", "read the next <=10 new messages (advances the cursor; -l/--limit N)"},
		{"ppz send TGT PAYLOAD", "publish a message to a pipe"},
		{"ppz reread TGT", "replay retained messages (never moves the cursor)"},
		{"ppz command H [INSTR]", "type INSTR into H.stdin, then a control key"},
		{"ppz subs SUBVERB", "per-session pipe subscriptions (ls/add/rm/wait/read)"},
		{"ppz schedule SUBVERB", "manage scheduled sends (ls/rm); create via ppz send --at/--every/--cron"},
		{"ppz chat", "live roster + per-agent/-pipe chat (full-screen)"},
	}},
	{"IDENTITIES", []helpRow{
		{"ppz source create H", "claim a bare message handle (auto-pipe: inbox)"},
		{"ppz agent create H", "create a handle and run an AI harness in it"},
		{"ppz source destroy PAT", "glob-destroy sources or pipes"},
	}},
	{"TERMINAL", []helpRow{
		{"ppz terminal share H", "run a shell/CMD in a pty bound to H"},
		{"ppz terminal watch H", "follow H.stdout live in a TUI"},
		{"ppz terminal read H", "render H.stdout (reread with --tty default)"},
	}},
	{"DAEMON", []helpRow{
		{"ppz daemon start", "start the local daemon"},
		{"ppz daemon stop", "stop the local daemon (idempotent)"},
		{"ppz daemon restart", "stop + start (use after 'ppz upgrade')"},
		{"ppz daemon logout", "clear the stored credential"},
	}},
	{"DAEMON STATE", []helpRow{
		{"ppz set handle H", "set this session's current handle"},
		{"ppz unset handle", "clear the current handle"},
		{"ppz get handle", "print the current handle"},
		{"ppz set namespace PATH", "set the current namespace (manifold)"},
		{"ppz unset namespace", "clear the namespace (back to root)"},
	}},
	{"PIPES", []helpRow{
		{"ppz pipe create [H.]NAME", "create a custom pipe"},
		{"ppz pipe destroy [H.]NAME", "destroy a pipe (--recursive for a tree)"},
	}},
	{"OTHER", []helpRow{
		{"ppz who", "list agents the daemon has seen heartbeats from"},
		{"ppz diagnostics", "introspect the daemon (works without login)"},
		{"ppz version", "print the binary's version + build sha"},
		{"ppz upgrade", "install the latest ppz CLI release"},
		{"ppz completion {bash|zsh}", "print a shell tab-completion script"},
	}},
}

// helpTitle is the one-line banner shown above the grouped help.
const helpTitle = "ppz — pipes for agents"

// renderTopLevel builds the grouped top-level help body. Signatures are
// padded into a common column with a 2-space gap so wrapUsageText's descCol
// detection reflows the summaries to terminal width.
func renderTopLevel() string {
	sigWidth := 0
	for _, g := range topLevelGroups {
		for _, r := range g.rows {
			if n := len([]rune(r.sig)); n > sigWidth {
				sigWidth = n
			}
		}
	}

	var b strings.Builder
	b.WriteString(helpTitle)
	b.WriteString("\n")
	for _, g := range topLevelGroups {
		b.WriteString("\n")
		b.WriteString(g.title)
		b.WriteString("\n")
		for _, r := range g.rows {
			pad := sigWidth - len([]rune(r.sig))
			fmt.Fprintf(&b, "  %s%s  %s\n", r.sig, strings.Repeat(" ", pad), r.summary)
		}
	}
	b.WriteString("\nRun 'ppz <command> --help' for details on any command.\n")
	b.WriteString("Topics: ppz help acks · ppz help sessions · ppz help globs\n")
	return b.String()
}

// wantsHelp reports whether args is asking for help: a -h / --help flag
// appearing before any "--" terminator. Stopping at "--" lets passthrough
// verbs forward help flags to the wrapped command — `ppz command H -- --help`
// and `ppz terminal share H -- cmd --help` must reach the command, not print
// ppz's help.
//
// It deliberately does NOT match the bare word "help": for a payload/handle-
// bearing verb that would swallow real input (`ppz send alice help` must send
// the message "help"; `ppz command H help` must type "help"). The bare-word
// form is served by top-level `ppz help <verb>` (root.go) and by group
// dispatchers, where the token sits in a fixed subverb slot, not a payload.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// printHelp renders the detailed help body for key (a verb path or topic),
// reflowed to the terminal width. An unknown key falls back to the grouped
// top-level help so a missing entry degrades gracefully rather than printing
// nothing.
func printHelp(w io.Writer, key string) {
	body, ok := helpTopics[key]
	if !ok {
		body = renderTopLevel()
	}
	fmt.Fprintln(w, wrapUsageText(body, cliproto.TerminalWidth()))
}

// usageExit prints a command's help body — which leads with its own `usage:`
// line — to stderr, then exits 2. It's the bad-args counterpart to
// `ppz <verb> --help`, which prints the identical body to stdout and exits 0:
// one source of truth (helpTopics), two exit codes.
func usageExit(key string) {
	printHelp(os.Stderr, key)
	os.Exit(2)
}

// groupHelp handles help requests for a command group dispatcher. It prints
// and returns true for:
//
//	ppz <group> --help | -h | help          → the group overview
//	ppz <group> <sub> … --help | -h | help   → "<group> <sub>" detail (if known)
//
// Callers return nil when it returns true. It does NOT fire for a bare
// `ppz <group>` (no args) — the dispatcher handles that as a usage error.
// wantsHelp's "--" stop means a passthrough like `ppz terminal share H --
// cmd --help` is not hijacked.
func groupHelp(group string, args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printHelp(os.Stdout, group)
		return true
	}
	if wantsHelp(args[1:]) {
		key := group + " " + args[0]
		if _, ok := helpTopics[key]; !ok {
			key = group
		}
		printHelp(os.Stdout, key)
		return true
	}
	return false
}

// helpTopics holds every detailed help body. Verb-path keys back both
// `ppz <verb> --help` and `ppz help <verb>`; topic keys back `ppz help
// <topic>`. Prose paragraphs are single source lines (see CONVENTION above)
// so wrapUsageText reflows them to width.
var helpTopics = map[string]string{
	// ---- Messaging verbs -------------------------------------------------
	"status": `usage: ppz status

Print the daemon's state: whether it's logged in, its pid + version, last token refresh, server URL, account, current handle, and NATS connection state. The first stop when something looks wrong. See 'ppz help sessions' for how the current handle is scoped per shell session.`,

	"ls": `usage: ppz ls [--json|--iso] [--watch] [PATTERN...]

List handles × pipes. With no PATTERN, lists everything the daemon knows about; PATTERNs glob the full <handle>.<pipe> (e.g. '*.inbox', 'alice.*'). A literal that matches no pipe warns — use a glob ('*' quoted, or % unquoted).

  --watch    block until unread arrives on a matching pipe, print a snapshot, and exit. Non-destructive (does not advance any cursor), so it's the wake-signal primitive for an agent monitor loop. See 'ppz help globs' for pattern rules.
  --json     emit one JSON object per row.
  --iso      render LAST as an RFC3339 timestamp instead of a relative age.`,

	"read": `usage: ppz read TGT [-l|--limit N --tail --json --tty --raw --bare]

Read NEW messages from <handle>.<pipe> and advance the session cursor — the agent inbox poll (like 'git log' showing only new commits). 'ppz read inbox' reads <current>.inbox. The retrospective/forensic counterpart is 'ppz reread', which carries --skip/--since (and a tail-N -l) and never moves the cursor.

Flood protection: delivers at most the next 10 unread (oldest first) per invocation. The cursor advances only past what was delivered, so nothing is skipped — a truncated read ends with a "(N more unread - run again to continue)" trailer and the next invocation picks up exactly where this one stopped. The trailer is suppressed under --raw/--json/--bare (script-stable output modes).

Default output on inbox-shaped pipes is the v0.23 tabular format:
    HH:MM:SS  <sender|->  <body>
where <body> is "[subject] payload" for user subjects, "ack:read → <id8>" for system ack messages (see 'ppz help acks'), or just the payload.

  -l, --limit N   deliver at most the next N oldest unread (head-N; default 10). -l 0 removes the cap and drains everything unread. NOTE: unlike 'reread -l' (tail-N over history), read's -l never skips — it pages. Mutually exclusive with --tail.
  --bare    force legacy payload-only output (script-stable opt-out from the tabular default on inbox-shaped pipes).
  --tail    drain unread (uncapped), then keep streaming live until SIGINT, advancing the cursor as messages arrive. Mutually exclusive with --tty.
  --tty     render the concatenated payloads through a virtual VT100 screen (best for <handle>.stdout from a wrapped pty). Mutually exclusive with --json.
  --raw     write payload bytes verbatim with no separator (byte-faithful; best for forensics and replay).
  --json    emit each message's full envelope as a JSON line.`,

	"send": `usage: ppz send TGT PAYLOAD [--subject S] [--in-reply-to ID] [--request-ack] [--at T | --every DUR | --cron EXPR]

Publish PAYLOAD to <handle>.<pipe>; a bare handle targets <handle>.inbox. The success line goes to STDERR (since v0.25 — scripts redirecting stdout no longer swallow it); exit 0 means delivery was confirmed.

  --subject S        envelope-level header. The 'ack:' prefix is reserved for system messages.
  --in-reply-to ID   thread / reply linkage to a prior message id.
  --request-ack      the receiver's daemon emits an 'ack:read' back to YOUR inbox when their cursor advances past your message (best-effort, non-blocking). See 'ppz help acks'.

Scheduled sends (mutually exclusive; the schedule is durable server-side state — it fires with this machine asleep):
  --at T             one-off at T: RFC3339, "YYYY-MM-DD HH:MM" (local), or +duration (+5m).
  --every DUR        recurring on an interval: Go duration, min 1s (15m, 1h30m).
  --cron EXPR        recurring at wall-clock times: 5-field cron ("0 10 * * MON"), in this device's timezone.
Manage with 'ppz schedule ls' / 'ppz schedule rm ID'.`,

	"reread": `usage: ppz reread TGT [-l|--limit N --skip N --since DUR --json --tty --raw --bare]

Forensic / replay: re-deliver retained messages from <handle>.<pipe>. Unlike 'ppz read', it ignores and never advances the session cursor, so you can inspect history without consuming it.

  -l, --limit N   limit to the last N messages.
  --skip N       skip the most recent N before applying -l/--limit.
  --since DUR    only messages newer than DUR ago (e.g. 10m, 2h).
  --json --tty --raw --bare   output modes, shared with 'ppz read'.`,

	"command": `usage: ppz command H [INSTR] [--claude|--cr|--crlf|--newline|--none]

Send INSTR to H.stdin (after a 100 ms delay), then send a trailing control sequence so the receiving harness submits the line. Use it to drive a pty handle created with 'ppz terminal share' / 'ppz agent create'.

Terminator (pick one; default --cr):
  --cr        carriage return \r — Enter for every non-claude harness (default).
  --claude    \x1b[13u — kitty-protocol Enter (claude harness).
  --crlf      carriage return + newline \r\n.
  --newline   newline \n.
  --none      send INSTR with no trailing control sequence.`,

	// ---- subs group ------------------------------------------------------
	"subs": `usage: ppz subs {ls|add|rm|wait|read}

A curated, per-session subset of pipe subjects — the agent inbox-monitor list. 'wait' and 'read' operate over this subset instead of the ls --watch firehose across every pipe.

  ppz subs ls [--json|--iso]          print the subscription set (table)
  ppz subs add <target>...            subscribe (idempotent)
  ppz subs rm [--force] <target>...   unsubscribe (own inbox guarded)
  ppz subs wait [--json|--iso]        block until a subscribed pipe has unread, then print only the unread row(s)
  ppz subs read [-l|--limit N] [--json|--raw|--tty|--bare]   read each subscribed pipe that has unread

A bare <target> is an uncollared pipe (read-style); use an explicit <handle>.<pipe> for a collared pipe such as an inbox.

'subs read' visits pipes in sorted order and applies 'ppz read's flood cap PER PIPE: at most the next 10 unread each (oldest first; -l/--limit N to change, -l 0 to drain everything). A pipe with more unread yields with a "(N more unread - run again to continue)" trailer inside its block, so one spammed pipe can't starve the rest or flood the reader's context — run 'subs read' again to page, or 'ppz read <target>' to drain one pipe. Cursors only advance past delivered messages, so paging never skips.`,

	"schedule": `usage: ppz schedule {ls|rm}

Manage scheduled sends (created via 'ppz send ... --at/--every/--cron'). The schedule is durable server-side state: it fires with your machine asleep or the daemon stopped. Fired one-offs and removed schedules leave the table.

  ppz schedule ls [--json|--iso]   list live schedules, soonest NEXT first
  ppz schedule rm ID               remove a schedule by the short id 'ls' shows`,

	// ---- Identities ------------------------------------------------------
	"source": `usage: ppz source {create|destroy} ...

  ppz source create HANDLE     claim a bare message-kind handle (auto-pipe: inbox)
  ppz source destroy PATTERN   glob-destroy sources or pipes

Run 'ppz source create --help' or 'ppz source destroy --help' for details.`,

	"source create": `usage: ppz source create HANDLE

Claim a bare message-kind handle (auto-pipe: inbox) and set it as the session's current handle. Use when you want a named actor identity without committing to a terminal or agent role. For a pty pipe set, run a terminal with 'ppz terminal share' (auto-creates the handle); for an agent harness, use 'ppz agent create'. Strict: errors if the handle already exists in the account.`,

	"source destroy": `usage: ppz source destroy PATTERN

Glob-destroy sources or pipes:
  bare pattern         → matching sources (and all their pipes)
  handle.pipe pattern  → matching pipes
  glob wildcards: * ? [abc]   (path.Match rules — see 'ppz help globs')

Examples: destroy '*' · destroy 'agent-*' · destroy '*.stdout' · destroy apple`,

	"terminal": `usage: ppz terminal {share|watch|control|lease|release|read} ...

  ppz terminal share H [-- CMD...]  run CMD (or $SHELL) in a pty bound to H
  ppz terminal watch H              follow H.stdout in an alt-screen TUI (read-only)
  ppz terminal control H            attach interactively: watch + forward keystrokes (holds the write-lease)
  ppz terminal lease H DURATION     acquire the advisory write-lease on H for DURATION
  ppz terminal release H            release the write-lease on H
  ppz terminal read H [flags]       render H.stdout (reread with --tty default)

Run 'ppz terminal <subverb> --help' for details.`,

	"terminal share": `usage: ppz terminal share H [-- CMD ...]

Run CMD (or $SHELL) in a pty bound to handle H — bidirectional: H.stdout is published and H.stdin is subscribed, so 'ppz command H' can drive it. Pass a command after '--'; flags after '--' go to that command, not to ppz.`,

	"terminal watch": `usage: ppz terminal watch H

Follow H.stdout live in an alt-screen TUI. Interactive — for scripted/agent use prefer 'ppz terminal read H' (a one-shot render).`,

	"terminal read": `usage: ppz terminal read H [reread-flags]

Wrapper for 'ppz reread H.stdout' with --tty as the default output mode (a vt10x screen render that rebuilds cumulative terminal state). Accepts the same flags as 'ppz reread'.`,

	"terminal control": `usage: ppz terminal control H

Attach interactively to the pty bound to handle H: follow H.stdout (like 'watch') AND forward your keystrokes to H.stdin, after acquiring the advisory write-lease. If another controller already holds the lease, control degrades to a read-only attach (streams output; keystrokes not forwarded). Ctrl-C / Ctrl-D detaches and releases the lease. See 'ppz help' and docs/WIRE.md §12 for the lease model.`,

	"terminal lease": `usage: ppz terminal lease H DURATION

Acquire the advisory write-lease on handle H for DURATION (Go duration, e.g. 60s, 5m). Blocks until the pty host grants or denies. On grant exits 0; if another controller holds it, prints 'held by <holder>' and exits 27 (E_LEASE_HELD). The lease coordinates cooperating writers to H.stdin — it is keyed on your PPZ_CURRENT_HANDLE and is advisory, not an authenticated boundary.`,

	"terminal release": `usage: ppz terminal release H

Release your write-lease on handle H. A release by anyone but the current holder is a no-op. Blocks briefly for the host's confirmation so scripts can sequence a follow-on send.`,

	"agent": `usage: ppz agent create NAME [PROMPT] [flags]

  ppz agent create NAME [PROMPT]   create a pty source NAME and run an AI harness

Run 'ppz agent create --help' for the harness/model switches.`,

	"agent create": `usage: ppz agent create NAME [PROMPT] [flags]

Create a pty-backed source NAME and run an AI harness in it. Default: --claude --opus.

  Harness:  --claude | --copilot | --codex | --agy | --pi
  Model:    --opus | --sonnet | --haiku   (claude only)
            --model X                       (any harness)
  --prompt-file PATH    read the prompt from PATH instead of the positional argument
  --new-window          open a fresh Terminal.app / iTerm2 window`,

	// ---- Setup -----------------------------------------------------------
	"daemon": `usage: ppz daemon {start|stop|restart|login|logout} ...

  ppz daemon start            start the local daemon
  ppz daemon stop             stop the local daemon (idempotent)
  ppz daemon restart          stop + start
  ppz daemon login URL        log the daemon into a server (browser device flow)
  ppz daemon logout           clear the stored credential

'ppz login URL' is a top-level shortcut for 'ppz daemon login'.`,

	"daemon start": `usage: ppz daemon start [--foreground]

Start the local daemon (the long-lived process that owns the NATS connection and serves the CLI over a unix socket). --foreground runs it in the current terminal instead of detaching (for debugging).`,

	"daemon stop": `usage: ppz daemon stop

Stop the local daemon. Idempotent — succeeds even if it isn't running.`,

	"daemon restart": `usage: ppz daemon restart

Stop then start the local daemon. Use after 'ppz upgrade' when 'ppz status' reports the daemon out of sync with the CLI.`,

	"daemon login": `usage: ppz daemon login URL [-apikey K] [-no-open]

Log the daemon into a server. By default runs the browser device flow — just give the URL. 'ppz login URL' is the top-level shortcut. See 'ppz login --help' for the flags.`,

	"daemon logout": `usage: ppz daemon logout

Clear the stored credential.`,

	"login": `usage: ppz login URL [-apikey K] [-no-open]

Log in to a server. By default this runs the browser device flow — just give the URL:

    ppz login pipescloud.io

Top-level shortcut for 'ppz daemon login' (matches the gh/kubectl/az login muscle memory).

  -apikey K   skip the browser flow and authenticate with an API key instead.
  -no-open    device flow: don't auto-open the browser (print the URL to visit).`,

	// ---- Daemon state ----------------------------------------------------
	"set": `usage: ppz set {handle HANDLE | namespace PATH}

  ppz set handle HANDLE     switch this session's current handle. The current handle is stamped as 'sender' on outgoing envelopes and is the implicit target for bare 'ppz read inbox' / 'ppz send TARGET'.
  ppz set namespace PATH    set this session's current namespace (manifold). New pipes from 'ppz pipe create LEAF' inherit it.`,

	"unset": `usage: ppz unset {handle | namespace}

  ppz unset handle      clear this session's current handle (the source row stays; only the per-session pointer is cleared).
  ppz unset namespace   clear the namespace; new pipes are created at the root manifold.`,

	"get": `usage: ppz get handle

Print the current handle to stdout. Exits 1 with empty output when no current handle is set, so $(ppz get handle) can detect "not set" via the return code.`,

	// ---- Pipes -----------------------------------------------------------
	"pipe": `usage: ppz pipe {create|destroy} ...

  ppz pipe create [HANDLE.]NAME [--ttl=DUR --max-msgs=N --max-bytes=B]
  ppz pipe destroy [HANDLE.]NAME [--recursive]

A bare NAME is created under the current namespace; prefix HANDLE. to collar it to a source.`,

	"pipe create": `usage: ppz pipe create [HANDLE.]NAME [--ttl=DUR --max-msgs=N --max-bytes=B]

Create a custom pipe. A bare NAME is created under the session's current namespace (manifold); prefix HANDLE. to collar it to a source.

  --ttl=DUR        retain messages for at most DUR (e.g. 24h, 30m).
  --max-msgs=N     cap retained messages at N.
  --max-bytes=B    cap retained bytes at B (accepts sizes like 64MiB, 1GB).`,

	"pipe destroy": `usage: ppz pipe destroy [HANDLE.]NAME [--recursive]

Destroy a pipe. --recursive removes a manifold subtree of pipes.`,

	// ---- Other -----------------------------------------------------------
	"who": `usage: ppz who [--json] [--online|--stale|--offline] [--harness=X] [--owner=X]

List every agent the local daemon has seen a heartbeat from, with online/stale/offline status, harness, model, host, os/arch, CREATED (uptime as a relative age) and OWNER (the source's creator, resolved at query time). Filters combine OR for status, AND for harness and owner.`,

	"chat": `usage: ppz chat

Full-screen live roster + chat. The left menu lists agents (online/stale/offline status, like 'ppz who') plus any uncollared pipes you add; selecting one opens a conversation. Agent DMs stitch your sends to <handle>.inbox together with the replies that land in your inbox; uncollared pipes show every sender on the shared subject.

  up/down or j/k  move     enter  open     a or +  add pipe     pgup/pgdn or wheel  scroll     esc  back     q  quit

Set a stable $PPZ_SESSION (e.g. ppz-tui) so it doesn't share a read cursor with a CLI 'ppz read'. Alias: 'ppz tui'.`,

	"diagnostics": `usage: ppz diagnostics [--json] [--since=DUR] [--bundle]

Introspect the daemon: NATS connection state, refresh timing, recent connection-state events (disconnect/reconnect/closed) and daemon lifecycle events, plus auto-detected anomaly patterns. Works WITHOUT login — the point is to introspect a sick daemon.

  --since=DUR   read on-disk history over the last DUR (e.g. 1h) instead of the in-memory ring.
  --bundle      write a support tarball (~/ppz-diag-<ts>.tgz) for a bug report.
  --json        machine-readable; patterns are first-class in the JSON.`,

	"version": `usage: ppz version

Print the binary's version and build sha.`,

	"upgrade": `usage: ppz upgrade

Download and install the latest ppz CLI release. Run 'ppz daemon restart' afterwards if 'ppz status' reports the daemon out of sync.`,

	"completion": `usage: ppz completion {bash|zsh}

Print a shell tab-completion script. Add 'eval "$(ppz completion bash)"' (or zsh) to your shell rc.`,

	// ---- Cross-cutting topics --------------------------------------------
	"acks": `Acks — read receipts (v0.25)

Use 'ppz send … --request-ack' when you need to know the recipient saw your message. Their daemon auto-emits an 'ack:read' envelope back to your inbox carrying in_reply_to=<your-msg-id>. The tabular read formatter renders these as 'ack:read → <id8>' so you can correlate at a glance.

Best-effort: a missing ack is indistinguishable from "not yet read". If you need strict guarantees, layer your own re-send-on-timeout. The 'ack:' subject prefix is reserved — the CLI and daemon both reject user attempts to set it (E_INVALID_SUBJECT).`,

	"sessions": `Sessions — current handle & subprocess identity

Each shell session has its own current handle, keyed off the calling tty. Subprocesses with no shared tty get a fresh session id per invocation, so a 'ppz source create' in one call won't be visible to the next — and 'ppz send --request-ack' will reject with E_NO_CURRENT_SOURCE.

Pin a stable id by exporting PPZ_SESSION=<id> at the agent's lifecycle level so all subsequent ppz calls share session state.`,

	"globs": `Globs — pattern matching

Patterns use path.Match rules (Go's filepath.Match):
  *       matches any run of non-separator characters
  ?       matches any single character
  [abc]   matches one character in the set

Where patterns apply:
  ppz ls [PATTERN]          globs the full <handle>.<pipe> (e.g. '*.inbox', 'alice.*')
  ppz source destroy PAT    bare → sources; handle.pipe → pipes
  ppz subs add <target>     literal or pattern subjects

Quote '*' in the shell (or use the % alias unquoted) so ppz sees the pattern, not a shell expansion. A literal that matches no pipe warns — use a glob.`,
}
