package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/term"
)

// cmdAgentGroup dispatches `ppz agent <subverb>`. `create` provisions a
// fresh pty source and runs a harness in it; `run` is the cut-down
// sibling that runs a harness in an already-set-up agent. The verb is
// grouped so we can grow it (destroy, list, etc.) without re-shaping the
// CLI surface.
func cmdAgentGroup(args []string) error {
	if groupHelp("agent", args) {
		return nil
	}
	if len(args) == 0 {
		printHelp(os.Stderr, "agent")
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		return cmdAgentCreate(args[1:])
	case "run":
		return cmdAgentRun(args[1:])
	case "prompt":
		return cmdAgentPrompt(args[1:])
	}
	fmt.Fprintf(os.Stderr, "ppz agent: unknown subcommand %q\n", args[0])
	os.Exit(2)
	return nil
}

// cmdAgentCreate is the wired-up command. Two execution paths:
//
//  1. Default (foreground): build the harness argv and call cmdTerminalShare
//     directly with `<handle> -- <argv>`. The current shell becomes the
//     agent's controlling terminal — Ctrl-C exits the agent.
//  2. --new-window: write the prompt to a temp file, build a `ppz terminal
//     share <handle> -- <harness> ... "$(cat <file>)"` shell command, and
//     hand it to osascript so a fresh Terminal.app/iTerm window runs it.
//     Returns immediately so the parent shell stays usable.
//
// Either way, `ppz terminal share` itself creates the source as KindPTY —
// we don't pre-create it. That matches the demo (../ppz-demo-1/setup.sh)
// which just runs `ppz terminal share <name>` per agent.
func cmdAgentCreate(args []string) error {
	spec, handle, err := resolveAgentSpec(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ppz agent create:", err)
		os.Exit(2)
	}
	if spec.newWindow {
		return runAgentInNewWindow(handle, spec)
	}
	return runAgentInForeground(handle, spec)
}

// runAgentInForeground hands off to cmdTerminalShare in-process. The
// argv shape is `<handle> -- <harness-argv...>` so terminal share runs
// the harness inside the wrapped pty. Before handing off, the agent
// identity env (PPZ_AGENT_HARNESS / PPZ_AGENT_MODEL) is exported into
// the current process so terminalShareEnv picks it up via os.Environ()
// — that's how heartbeats learn what harness they're stamping.
func runAgentInForeground(handle string, spec agentSpec) error {
	argv, err := buildAgentArgv(spec)
	if err != nil {
		return err
	}
	setAgentEnv(spec)
	shareArgs := append([]string{handle, "--"}, argv...)
	return cmdTerminalShare(shareArgs)
}

// cmdAgentPrompt is the most cut-down agent verb: it resolves the same
// spec as `agent create` / `agent run` but, instead of launching
// anything, echoes the initial prompt those verbs would boot the harness
// with. Useful to preview or pipe the boot prompt (e.g. `ppz agent prompt
// ash --codex | pbcopy`). No provisioning, no tty guard, no exec.
func cmdAgentPrompt(args []string) error {
	prompt, err := agentPromptFor(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ppz agent prompt:", err)
		os.Exit(2)
	}
	fmt.Println(prompt)
	return nil
}

// agentPromptFor resolves the initial prompt `agent prompt` echoes. It is
// the pure core of cmdAgentPrompt (returns the string rather than
// printing it) so the resolution is unit-testable. Flags that don't shape
// the prompt (--model, --new-window) are parsed but inert — only the
// harness (which selects the default prompt) and the prompt source
// (positional / --prompt-file / default) affect the result.
func agentPromptFor(args []string) (string, error) {
	spec, _, err := resolveAgentSpecVerb("prompt", args)
	if err != nil {
		return "", err
	}
	return spec.prompt, nil
}

// cmdAgentRun is the cut-down sibling of `agent create`: it runs an AI
// harness (with the initial prompt) inside an agent whose source is
// already set up, instead of provisioning a fresh one. It shares create's
// flag surface (harness/model/prompt/--prompt-file) but is foreground-
// only — no --new-window — and always requires an explicit handle.
func cmdAgentRun(args []string) error {
	spec, handle, err := resolveAgentSpecVerb("run", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ppz agent run:", err)
		os.Exit(2)
	}
	if err := agentRunPreflight(spec, term.IsTerminal(int(os.Stdin.Fd()))); err != nil {
		fmt.Fprintln(os.Stderr, "ppz agent run:", err)
		os.Exit(2)
	}
	return runAgentRun(handle, spec)
}

// agentRunPreflight validates a resolved `agent run` invocation before
// any daemon call or process launch. It is a pure function (the tty
// state is passed in) so the two guards are unit-testable without a real
// terminal:
//
//   - --new-window is a create-only affordance; `agent run` is
//     foreground-only in this cut, so reject it rather than silently
//     ignore a flag the user clearly meant.
//   - `agent run` runs the harness in the *current* shell. If stdin is
//     not a tty (a pipe, a redirect, a scripted runner) the interactive
//     harness would boot into a terminal it can't be driven through, so
//     fail fast with a message that names the cause.
func agentRunPreflight(spec agentSpec, stdinIsTTY bool) error {
	if spec.newWindow {
		return fmt.Errorf("--new-window is not supported by `agent run`; use `ppz agent create` to open a new window")
	}
	if !stdinIsTTY {
		return fmt.Errorf("stdin is not a tty: `agent run` launches an interactive harness in the current terminal — run it directly in your shell, not through a pipe or redirect")
	}
	return nil
}

// runAgentRun mirrors runAgentInForeground but hands off to the ensure-
// pty share path (terminalShareExisting) so the already-provisioned
// source is upgraded in place rather than CREATEd afresh. The initial
// prompt reaches the harness as an argv element from buildAgentArgv, and
// setAgentEnv exports the agent identity so heartbeats stay stamped —
// both identical to create.
func runAgentRun(handle string, spec agentSpec) error {
	argv, err := buildAgentArgv(spec)
	if err != nil {
		return err
	}
	setAgentEnv(spec)
	shareArgs := append([]string{handle, "--"}, argv...)
	return terminalShareExisting(shareArgs)
}

// agentEnvPairs returns the agent-identity env-var assignments the pty
// wrapper reads to stamp every heartbeat. Always two keys so the wire
// schema stays consistent regardless of harness; model may be empty
// when the agent harness has no default (e.g. copilot/codex without
// --model).
func agentEnvPairs(spec agentSpec) []string {
	return []string{
		"PPZ_AGENT_HARNESS=" + spec.harness,
		"PPZ_AGENT_MODEL=" + spec.model,
	}
}

// setAgentEnv exports agentEnvPairs into the current process env. The
// foreground path relies on this — cmdTerminalShare is called in-
// process, and terminalShareEnv appends os.Environ() to the wrapped
// child's env, so the agent identity flows through transitively. The
// new-window path can't share process state with the spawned terminal,
// so it injects the same pairs as a shell `env KEY=VAL ...` prefix
// instead — see runAgentInNewWindow.
func setAgentEnv(spec agentSpec) {
	for _, kv := range agentEnvPairs(spec) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		_ = os.Setenv(k, v)
	}
}

// runAgentInNewWindow asks the host's window-manager to open a new
// terminal running `ppz terminal share <handle> -- <harness>
// [...] '<prompt>'`. The prompt is bash-single-quoted inline via
// buildHarnessSpawnArgv — see that helper for why the previous
// `"$(cat FILE)"` temp-file round-trip was abandoned.
//
// Backend per platform:
//   - darwin: osascript → Terminal.app / iTerm2 (see buildNewWindowScript)
//   - linux (WSL):   wt.exe + wsl.exe (see buildWSLNewWindowArgv)
//   - linux (native): $TERMINAL or probed emulator (see buildLinuxNewWindowArgv)
func runAgentInNewWindow(handle string, spec agentSpec) error {
	argv, err := buildHarnessSpawnArgv(spec)
	if err != nil {
		return err
	}

	// Inherit the parent shell's cwd so the spawned harness boots in
	// the folder the user already trusts. Without this, the new
	// terminal opens in $HOME and claude shows a "trust this folder?"
	// dialog on every run.
	cwd, _ := os.Getwd()
	envPairs := agentEnvPairs(spec)

	switch runtime.GOOS {
	case "darwin":
		script := buildNewWindowScript(os.Getenv("TERM_PROGRAM"), handle, cwd, envPairs, argv)
		return runWindowCmd("osascript", []string{"-e", script})
	case "linux":
		procVersion, _ := os.ReadFile("/proc/version")
		if isWSL(string(procVersion)) {
			body := buildWSLScript(handle, cwd, envPairs, argv)
			scriptPath, err := writeAgentSpawnScript("", handle, body)
			if err != nil {
				return err
			}
			cmdArgv, err := buildWSLNewWindowArgv(os.Getenv("WSL_DISTRO_NAME"), scriptPath)
			if err != nil {
				return err
			}
			return runWindowCmd(cmdArgv[0], cmdArgv[1:])
		}
		terminal, err := selectLinuxTerminal(os.Getenv("TERMINAL"), func(name string) bool {
			_, err := exec.LookPath(name)
			return err == nil
		})
		if err != nil {
			return err
		}
		cmdArgv, err := buildLinuxNewWindowArgv(terminal, handle, cwd, envPairs, argv)
		if err != nil {
			return err
		}
		return runWindowCmd(cmdArgv[0], cmdArgv[1:])
	}
	return fmt.Errorf("--new-window: unsupported platform %q", runtime.GOOS)
}

// runWindowCmd is a thin wrapper around exec.Command that wires
// stdout/stderr to the parent — used by all three --new-window
// backends so they handle process-spawn the same way.
func runWindowCmd(name string, args []string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// agentSpec is the resolved input to buildAgentArgv. The CLI flag parser
// resolves --claude/--codex/etc into harness, --opus/--sonnet/--haiku and
// --model into a single model string, and the positional prompt or
// --prompt-file into prompt.
type agentSpec struct {
	harness   string
	model     string
	prompt    string
	newWindow bool
}

// defaultAgentPrompt returns the orientation prompt sent when the user
// supplies no positional prompt and no --prompt-file. Templated on the
// handle so the wake recipe can pin PPZ_SESSION=<handle> inline, and
// dispatched on harness so each harness gets a prompt that names its
// own primitives (claude → Monitor + PushNotification; copilot → bash
// detach: true; codex/agy/pi → foreground `ppz subs wait` loop).
//
// Every harness's prompt steers the agent at `ppz subs wait` / `ppz
// subs read` rather than the older `ppz ls --watch <handle>.inbox` /
// `ppz read inbox` pair: the per-session subscription set (with the
// agent's own inbox auto-subscribed at agent-create time) already
// defines the watched scope, so the wake verb takes no pattern arg,
// and `subs read` consumes every subscribed pipe in one go.
//
// Inheriting PPZ_SESSION from the parent shell is unreliable: some
// harness/Monitor combinations don't propagate env to subprocesses
// (Claude Code v2.1.143 was observed dropping it on Monitor's bash),
// and a Monitor subprocess with no PPZ_SESSION resolves a fresh
// tty-less session id the daemon has never seen — every ppz call
// inside then fails E_NO_CURRENT_SOURCE. Setting PPZ_SESSION inline
// in the recipe makes it robust to any future env-strip behavior.
func defaultAgentPrompt(handle, harness string) string {
	switch harness {
	case "claude":
		return claudeAgentPrompt(handle)
	case "copilot":
		return copilotAgentPrompt(handle)
	}
	// codex / agy / pi (and any future harness) share a generic
	// foreground "subs wait is a wake signal" loop that names no
	// harness-specific tools. agy and pi piggyback on this until
	// their own background/notification primitives are confirmed.
	return codexAgentPrompt(handle)
}

// claudeAgentPrompt names Claude Code's Monitor and PushNotification
// harness tools to turn the agent push-driven on subscribed-pipe
// arrivals. The recipe — `ppz subs wait` wrapped in a `while true …
// sleep 60` loop — is throttled because `ppz subs wait` is level-
// triggered: a subscribed pipe with persistent unread would re-arm
// the watch immediately and flood the agent with duplicate
// notifications until something runs `ppz subs read` to clear the
// cursor.
//
// Three hard-won details encoded in the prose:
//
//  1. The wake verb takes NO argument. `ppz subs wait` is already
//     scoped by the per-session subscription set: the agent's own
//     `<handle>.inbox` is auto-subscribed at agent-create, and any
//     rooms the agent joins via `ppz subs add` extend the set.
//     Passing a positional pattern to `subs wait` is not a valid
//     CLI shape and would re-introduce the old "watch wakes on its
//     own heartbeat/stdout noise" failure mode.
//  2. The Monitor must run `ppz subs wait` ONLY, never `ppz read`
//     and never `ppz subs read`. Both advance the session cursor
//     and consume messages, so the agent's own foreground
//     `ppz subs read` would then come back empty. `ppz reread` is
//     named as the recovery for a pipe already drained by a stray
//     read.
//  3. A worked example pins how to join/leave a room — agents who
//     only see the cheat-sheet verb signatures consistently asked
//     the user "how do I join a room?" instead of running the
//     verb themselves.
//
// Authored with `~` standing in for backticks (raw Go strings can't
// contain backticks) and `<HANDLE>` for the substitution slot, both
// swapped at render time — same convention as codexAgentPrompt.
func claudeAgentPrompt(handle string) string {
	body := `You are an agent running inside a ppz (pipes) pty. Your handle is "<HANDLE>". Your terminal output is published to <HANDLE>.stdout. Other agents can reach you via <HANDLE>.inbox.

Useful commands:
  ppz status                              find out which source you are
  ppz who                                 see which other agents are online
  ppz subs ls                             list pipes you are subscribed to
  ppz subs add <target>...                subscribe to a pipe (e.g. a chat room)
  ppz subs rm <target>...                 unsubscribe from a pipe
  ppz subs wait                           block until any subscribed pipe has unread
  ppz subs read                           read all unread subscribed pipes (banner per pipe)
  ppz send <handle> <text>                send a message to another agent
  ppz send <pipe> <text>                  send a message to a pipe
  ppz send <target> <text> --at <when>    schedule a one-off send (+5m, "2026-07-08 09:55", or RFC3339)
  ppz send <target> <text> --every <dur>  schedule a recurring send (e.g. 15m, 1h)
  ppz send <target> <text> --cron <expr>  schedule at wall-clock times (e.g. "0 10 * * MON")
  ppz schedule ls                         list your scheduled sends
  ppz schedule rm <id>                    cancel a scheduled send

To join a chat room and start receiving its messages:
    ppz subs add project-room
To leave a room when done:
    ppz subs rm project-room
Your own <HANDLE>.inbox is auto-subscribed; you never need to add it.

Scheduled sends are durable server-side state: they fire even if you exit or this machine sleeps. Use them for reminders to yourself (~ppz send <HANDLE> "check the build" --at +30m~), delayed follow-ups to peers, or recurring check-ins to a room. Messages fired from a schedule carry a schedule_id field (visible with --json reads) so you can tell them apart from live sends.

Create a persistent Monitor running ~while true; do PPZ_SESSION=<HANDLE> ppz subs wait 2>/dev/null; sleep 60; done~ that fires a PushNotification on each wakeup. When the PushNotification fires, run ~ppz subs read~ in your own turn — it prints each unread subscribed pipe under a ~=== <target> ===~ banner and advances the cursor so the next ~subs wait~ only wakes on truly new arrivals. PPZ_SESSION is set inline so the subprocess works even if env isn't inherited. The trailing sleep 60 throttles the loop: ~ppz subs wait~ is level-triggered, so without it the loop would re-fire immediately on every iteration as long as any subscribed pipe still has unread, flooding you with duplicate events until you ~ppz subs read~ to clear them.

Keep the Monitor to ~ppz subs wait~ only — never run ~ppz read~ inside the loop, and never run ~ppz subs read~ inside the loop. Both advance your session cursor and consume messages, so a background read drains the pipes and your own foreground ~ppz subs read~ then comes back empty. When the Monitor fires, do the ~ppz subs read~ in your own turn. If a stray background read has already drained a pipe, ~ppz reread <pipe>~ replays its full retained history without advancing the cursor.`
	body = strings.ReplaceAll(body, "~", "`")
	body = strings.ReplaceAll(body, "<HANDLE>", handle)
	return body
}

// copilotAgentPrompt is the GitHub Copilot CLI variant. Two
// copilot-specific concerns:
//
//  1. The "You are the GitHub Copilot CLI. You respond to natural
//     language instructions …" framing line stops copilot from
//     trying to `exec` whatever arrives in inbox as a raw shell
//     command — copilot was observed doing exactly that without it.
//  2. `bash` with `detach: true` is copilot's analogue of claude's
//     Monitor — a background-process tool that lets the watch loop
//     run independently of the agent's foreground turn. Naming the
//     option explicitly avoids the agent running the loop
//     synchronously and stalling the session.
//
// The subs-wait recipe, room join/leave example, and never-
// background-read guidance mirror claudeAgentPrompt — see its doc
// comment for the rationale. Authored with `~`/`<HANDLE>` swapped at
// render time.
func copilotAgentPrompt(handle string) string {
	body := `You are an agent running inside a ppz (pipes) pty. Your handle is "<HANDLE>". Your terminal output is published to <HANDLE>.stdout. Other agents can reach you via <HANDLE>.inbox.

You are the GitHub Copilot CLI. You respond to natural language instructions — messages sent to your inbox should be phrased as conversational prompts, not raw shell commands.

Useful commands:
  ppz status                              find out which source you are
  ppz who                                 see which other agents are online
  ppz subs ls                             list pipes you are subscribed to
  ppz subs add <target>...                subscribe to a pipe (e.g. a chat room)
  ppz subs rm <target>...                 unsubscribe from a pipe
  ppz subs wait                           block until any subscribed pipe has unread
  ppz subs read                           read all unread subscribed pipes (banner per pipe)
  ppz send <handle> <text>                send a message to another agent
  ppz send <pipe> <text>                  send a message to a pipe
  ppz send <target> <text> --at <when>    schedule a one-off send (+5m, "2026-07-08 09:55", or RFC3339)
  ppz send <target> <text> --every <dur>  schedule a recurring send (e.g. 15m, 1h)
  ppz send <target> <text> --cron <expr>  schedule at wall-clock times (e.g. "0 10 * * MON")
  ppz schedule ls                         list your scheduled sends
  ppz schedule rm <id>                    cancel a scheduled send

To join a chat room and start receiving its messages:
    ppz subs add project-room
To leave a room when done:
    ppz subs rm project-room
Your own <HANDLE>.inbox is auto-subscribed; you never need to add it.

Scheduled sends are durable server-side state: they fire even if you exit or this machine sleeps. Use them for reminders to yourself (~ppz send <HANDLE> "check the build" --at +30m~), delayed follow-ups to peers, or recurring check-ins to a room. Messages fired from a schedule carry a schedule_id field (visible with --json reads) so you can tell them apart from live sends.

Use your bash tool with detach: true to create a persistent Monitor running:
    while true; do PPZ_SESSION=<HANDLE> ppz subs wait 2>/dev/null; sleep 60; done
This should fire a PushNotification on each wakeup. When the PushNotification fires, run ~ppz subs read~ in your own turn — it prints each unread subscribed pipe under a ~=== <target> ===~ banner and advances the cursor so the next ~subs wait~ only wakes on truly new arrivals. PPZ_SESSION is set inline so the subprocess works even if env isn't inherited. The trailing sleep 60 throttles the loop: ~ppz subs wait~ is level-triggered, so without it the loop would re-fire immediately on every iteration as long as any subscribed pipe still has unread, flooding you with duplicate events until you ~ppz subs read~ to clear them.

Keep the Monitor to ~ppz subs wait~ only — never run ~ppz read~ inside the loop, and never run ~ppz subs read~ inside the loop. Both advance your session cursor and consume messages, so a background read drains the pipes and your own foreground ~ppz subs read~ then comes back empty. When the Monitor fires, do the ~ppz subs read~ in your own turn. If a stray background read has already drained a pipe, ~ppz reread <pipe>~ replays its full retained history without advancing the cursor.`
	body = strings.ReplaceAll(body, "~", "`")
	body = strings.ReplaceAll(body, "<HANDLE>", handle)
	return body
}

// codexAgentPrompt is the foreground-watch variant used by codex
// itself and shared, for now, by agy and pi. None of these
// harnesses have a confirmed push primitive, so instead of running
// `ppz subs wait` in a detached loop we tell the agent to *itself*
// block on it when idle and treat the return as a wake signal that
// must be followed by `ppz subs read` before the next wait —
// otherwise the same unread snapshot re-wakes the agent indefinitely.
//
// The body is authored with `~` standing in for backticks (raw Go
// strings can't contain backticks) and `<HANDLE>` standing in for
// the substitution slot, both swapped at render time. The
// command-cheat-sheet column is aligned to col 36 (vs col 28 on
// claude/copilot) to keep CommandColumnIsAligned happy with
// codex-style longer descriptions; the wider column is consistent
// with the previous codex prompt's table.
func codexAgentPrompt(handle string) string {
	body := `You are an agent running inside a ppz (pipes) pty. Your handle is "<HANDLE>". Your terminal output is published to <HANDLE>.stdout. Other agents can reach you via <HANDLE>.inbox.

Useful commands:
  ppz status                              show daemon state and your current handle
  ppz who                                 see which other agents are online
  ppz subs ls                             list pipes you are subscribed to
  ppz subs add <target>...                subscribe to a pipe (e.g. a chat room)
  ppz subs rm <target>...                 unsubscribe from a pipe
  ppz subs wait                           block until any subscribed pipe has unread
  ppz subs read                           read every subscribed pipe that has unread
  ppz send <handle> <text>                send a message to another agent
  ppz send <pipe> <text>                  send a message to a pipe
  ppz send <target> <text> --at <when>    schedule a one-off send (+5m, "2026-07-08 09:55", or RFC3339)
  ppz send <target> <text> --every <dur>  schedule a recurring send (e.g. 15m, 1h)
  ppz send <target> <text> --cron <expr>  schedule at wall-clock times (e.g. "0 10 * * MON")
  ppz schedule ls                         list your scheduled sends
  ppz schedule rm <id>                    cancel a scheduled send

To join a chat room and start receiving its messages:
    ppz subs add project-room
To leave a room when done:
    ppz subs rm project-room
Your own <HANDLE>.inbox is auto-subscribed; you never need to add it.

Scheduled sends are durable server-side state: they fire even if you exit or this machine sleeps. Use them for reminders to yourself (~ppz send <HANDLE> "check the build" --at +30m~), delayed follow-ups to peers, or recurring check-ins to a room. Messages fired from a schedule carry a schedule_id field (visible with --json reads) so you can tell them apart from live sends.

Operational guidance:
  At the start of each task, run:
    ppz subs read

  When you are idle and expected to wait for more work, run:
    PPZ_SESSION=<HANDLE> ppz subs wait

  ~ppz subs wait~ blocks until any subscribed pipe has unread, prints the unread row(s), and exits. After it returns, run ~ppz subs read~ to actually consume the unread messages — that command prints each pipe under a ~=== <target> ===~ banner and advances the cursor so the next ~subs wait~ only wakes on truly new arrivals.

  Because ~ppz subs wait~ is level-triggered (it returns immediately whenever any subscribed pipe still has unread), do not put it in a tight loop. After it wakes, run ~ppz subs read~ before waiting again, otherwise the same unread message may wake you repeatedly.

  Do not run ~ppz subs read~ or ~ppz read~ in a background loop: both advance your session cursor and consume messages, so a foreground ~ppz subs read~ would then come back empty. If a stray read has already drained a pipe, ~ppz reread <pipe>~ replays its full retained history without advancing the cursor.

  If a sender is visible and the message requires acknowledgement, reply with:
    ppz send <sender> <reply>

The important distinction is: subs wait is a wake signal, not the read step. You should wake, run ~ppz subs read~, do the work, then return to ~ppz subs wait~.`
	body = strings.ReplaceAll(body, "~", "`")
	body = strings.ReplaceAll(body, "<HANDLE>", handle)
	return body
}

// buildHarnessSpawnArgv returns the harness argv for the --new-window
// spawn path. Identical to buildAgentArgv except the prompt element is
// bash-single-quoted so it inlines safely into the `bash -lc <script>`
// invocation we hand to the spawned terminal.
//
// Why not the old `"$(cat FILE)"` round-trip: when the WSL backend
// invokes wt.exe, Windows' argv parser sees our command line and
// strips the outer double quotes around the expansion before wt.exe
// hands the remainder to bash. Bash then sees unquoted `$(cat FILE)`
// and word-splits the file contents — the harness ends up receiving
// only the first word of the prompt (reproduced on this WSL2 box: a
// multi-line prompt arrived as the literal string "You").
// Single-quoting avoids the round-trip entirely: single quotes are
// inert at every layer (Windows' argv parser, AppleScript, bash) so
// any prompt content survives intact.
func buildHarnessSpawnArgv(spec agentSpec) ([]string, error) {
	quoted := spec
	if spec.prompt != "" {
		quoted.prompt = bashSingleQuote(spec.prompt)
	}
	return buildAgentArgv(quoted)
}

// buildAgentArgv returns the argv that runs *inside* the wrapped pty
// (i.e. the part after the `--` to `ppz terminal share`). It does not
// include `ppz terminal share <handle> --` — that prefix is the caller's
// responsibility (either via cmdTerminalShare directly, or as a string
// composed for osascript).
func buildAgentArgv(spec agentSpec) ([]string, error) {
	switch spec.harness {
	case "claude":
		argv := []string{"claude", "--dangerously-skip-permissions"}
		if spec.model != "" {
			argv = append(argv, "--model", spec.model)
		}
		if spec.prompt != "" {
			argv = append(argv, spec.prompt)
		}
		return argv, nil
	case "copilot":
		// copilot rejects a positional prompt with "Invalid command
		// format" — it tries to dispatch the first positional as a
		// subcommand. The initial prompt must arrive via `-i <prompt>`
		// (interactive mode with prompt). --yolo enables all
		// permissions so the unattended agent can act without per-tool
		// approval prompts.
		argv := []string{"copilot", "--yolo"}
		if spec.model != "" {
			argv = append(argv, "--model", spec.model)
		}
		if spec.prompt != "" {
			argv = append(argv, "-i", spec.prompt)
		}
		return argv, nil
	case "codex":
		// --dangerously-bypass-approvals-and-sandbox is codex's analogue
		// of claude's --dangerously-skip-permissions: it disables the
		// default seatbelt sandbox (CODEX_SANDBOX=seatbelt) which would
		// otherwise block the agent from reaching the host ppz daemon
		// (ppz status reports "daemon: not running" from inside the
		// sandbox even when the daemon is healthy). The agent is
		// unattended in a pty, so an interactive approval prompt would
		// just stall — bypass both.
		argv := []string{"codex", "--dangerously-bypass-approvals-and-sandbox"}
		if spec.model != "" {
			argv = append(argv, "--model", spec.model)
		}
		if spec.prompt != "" {
			argv = append(argv, spec.prompt)
		}
		return argv, nil
	case "agy":
		// Google Antigravity (the replacement for the deprecated
		// gemini CLI). agy has no `--model` flag — that's enforced
		// upstream in resolveAgentSpec, so spec.model is guaranteed
		// empty by the time we get here. The initial prompt arrives
		// via `-i <prompt>` (interactive mode that continues the
		// session after the prompt completes — same pattern as
		// copilot's `-i`), and `--dangerously-skip-permissions`
		// auto-approves tool prompts so the unattended agent can act
		// without a human at the terminal.
		argv := []string{"agy", "--dangerously-skip-permissions"}
		if spec.prompt != "" {
			argv = append(argv, "-i", spec.prompt)
		}
		return argv, nil
	case "pi":
		argv := []string{"pi"}
		if spec.model != "" {
			argv = append(argv, "--model", spec.model)
		}
		if spec.prompt != "" {
			argv = append(argv, spec.prompt)
		}
		return argv, nil
	}
	return nil, fmt.Errorf("unknown harness %q", spec.harness)
}

// resolveAgentSpec parses the args passed to `ppz agent create` and
// produces (spec, handle, error). It handles:
//
//   - harness selection: --claude (default) | --copilot | --codex |
//     --agy | --pi (mutually exclusive)
//   - model selection: claude shortcuts --opus / --sonnet / --haiku
//     (mutually exclusive, claude-only) OR --model X (claude/copilot/
//     codex/pi). agy has no --model flag, so combining --agy with
//     --model errors.
//     Combining a claude shortcut with --model also errors.
//   - prompt selection: positional <prompt> argument OR --prompt-file
//     <path>. If neither is given, defaultAgentPrompt is used.
//
// The handle is the first positional argument; the (optional) prompt is
// the second.
func resolveAgentSpec(args []string) (agentSpec, string, error) {
	return resolveAgentSpecVerb("create", args)
}

// resolveAgentSpecVerb is the shared flag parser behind both `agent
// create` and `agent run`. The two verbs take identical flags/positionals
// — the only difference is the verb woven into the missing-handle usage
// error (and the FlagSet name), so the message echoes back the command
// the user actually typed.
func resolveAgentSpecVerb(verb string, args []string) (agentSpec, string, error) {
	fs := flag.NewFlagSet("agent "+verb, flag.ContinueOnError)
	fs.SetOutput(devNull{})

	var (
		fClaude, fCopilot, fCodex, fAgy, fPi bool
		fOpus, fSonnet, fHaiku               bool
		fNewWindow                           bool
		fModel, fPromptFile                  string
	)
	fs.BoolVar(&fClaude, "claude", false, "use the claude harness (default)")
	fs.BoolVar(&fCopilot, "copilot", false, "use the copilot harness")
	fs.BoolVar(&fCodex, "codex", false, "use the codex harness")
	fs.BoolVar(&fAgy, "agy", false, "use the agy (Google Antigravity) harness")
	fs.BoolVar(&fPi, "pi", false, "use the pi harness")
	fs.BoolVar(&fOpus, "opus", false, "claude shortcut: --model opus")
	fs.BoolVar(&fSonnet, "sonnet", false, "claude shortcut: --model sonnet")
	fs.BoolVar(&fHaiku, "haiku", false, "claude shortcut: --model haiku")
	fs.BoolVar(&fNewWindow, "new-window", false, "open a new Terminal.app/iTerm2 window via osascript")
	fs.StringVar(&fModel, "model", "", "model passed to the harness")
	fs.StringVar(&fPromptFile, "prompt-file", "", "read the prompt from a file")

	// Pre-split flags from positionals so flag order doesn't matter
	// (matches cmdCommand's pattern). --prompt-file and --model carry
	// values, so we step over those.
	valueFlags := map[string]bool{"--model": true, "-model": true, "--prompt-file": true, "-prompt-file": true}
	var flagArgs, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if valueFlags[a] && !strings.Contains(a, "=") && i+1 < len(args) {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
			continue
		}
		rest = append(rest, a)
	}
	if err := fs.Parse(flagArgs); err != nil {
		return agentSpec{}, "", err
	}

	// Harness mutual exclusion. Default to claude when none given.
	harnessFlags := []struct {
		set  bool
		name string
	}{
		{fClaude, "claude"}, {fCopilot, "copilot"}, {fCodex, "codex"},
		{fAgy, "agy"}, {fPi, "pi"},
	}
	var picked []string
	for _, h := range harnessFlags {
		if h.set {
			picked = append(picked, h.name)
		}
	}
	var harness string
	switch len(picked) {
	case 0:
		harness = "claude"
	case 1:
		harness = picked[0]
	default:
		return agentSpec{}, "", fmt.Errorf("only one of --claude/--copilot/--codex/--agy/--pi may be set; got %v", picked)
	}

	// Claude model-shortcut mutual exclusion.
	shortcutCount := 0
	var shortcutModel string
	for _, s := range []struct {
		set  bool
		name string
	}{{fOpus, "opus"}, {fSonnet, "sonnet"}, {fHaiku, "haiku"}} {
		if s.set {
			shortcutCount++
			shortcutModel = s.name
		}
	}
	if shortcutCount > 1 {
		return agentSpec{}, "", fmt.Errorf("only one of --opus/--sonnet/--haiku may be set")
	}
	if shortcutCount == 1 && fModel != "" {
		return agentSpec{}, "", fmt.Errorf("--%s and --model are mutually exclusive", shortcutModel)
	}
	if shortcutCount == 1 && harness != "claude" {
		return agentSpec{}, "", fmt.Errorf("--%s is claude-only; use --model with --%s", shortcutModel, harness)
	}

	// agy has no model-selection flag (verified against `agy --help`),
	// so silently dropping a --model the user supplied would surprise
	// them. Reject the combination explicitly.
	if harness == "agy" && fModel != "" {
		return agentSpec{}, "", fmt.Errorf("--agy does not accept --model: agy has no model-selection flag")
	}

	// Resolve final model string.
	model := fModel
	if shortcutModel != "" {
		model = shortcutModel
	}
	if model == "" && harness == "claude" {
		model = "opus" // documented default per CLI design
	}

	// Positional: <handle> [<prompt>].
	if len(rest) == 0 {
		return agentSpec{}, "", fmt.Errorf("missing handle: ppz agent %s <name> [<prompt>] [flags...]", verb)
	}
	handle := rest[0]
	var positionalPrompt string
	if len(rest) > 1 {
		positionalPrompt = strings.Join(rest[1:], " ")
	}

	if positionalPrompt != "" && fPromptFile != "" {
		return agentSpec{}, "", fmt.Errorf("positional prompt and --prompt-file are mutually exclusive")
	}

	prompt := positionalPrompt
	if fPromptFile != "" {
		body, err := os.ReadFile(fPromptFile)
		if err != nil {
			return agentSpec{}, "", fmt.Errorf("--prompt-file %s: %w", fPromptFile, err)
		}
		prompt = string(body)
	}
	if prompt == "" {
		prompt = defaultAgentPrompt(handle, harness)
	}

	return agentSpec{harness: harness, model: model, prompt: prompt, newWindow: fNewWindow}, handle, nil
}

// buildNewWindowScript returns the osascript fragment that opens a new
// terminal window (Terminal.app or iTerm2 depending on TERM_PROGRAM)
// running `ppz terminal share <handle> -- <argv...>`. The argv is joined
// with spaces; multi-line/special-char prompts must be referenced via
// `$(cat /tmp/...)` to avoid shell-quoting nightmares — see
// cmdAgentCreate's --new-window path which writes a temp file.
//
// When cwd is non-empty the shell command is prefixed with
// `cd '<bash-quoted-cwd>' && ` so the spawned harness boots in the
// folder the parent shell was running in. macOS otherwise opens the new
// Terminal window in $HOME, and claude treats trust per-folder, so every
// run pops a "trust this folder?" dialog. Empty cwd is a graceful no-op.
func buildNewWindowScript(termProgram, handle, cwd string, envPairs []string, argv []string) string {
	cmd := shareInvocation(handle, envPairs, argv)
	if cwd != "" {
		cmd = "cd " + bashSingleQuote(cwd) + " && " + cmd
	}
	switch termProgram {
	case "iTerm.app":
		// iTerm2's "current session of current window" creates a new
		// session in the existing window unless we explicitly request a
		// new window. `create window with default profile` does the
		// right thing.
		return `tell application "iTerm"
    activate
    set newWindow to (create window with default profile)
    tell current session of newWindow to write text "` + escapeAppleScript(cmd) + `"
end tell`
	default:
		// Apple_Terminal and unknowns fall through to Terminal.app —
		// matches the demo's behaviour.
		return `tell application "Terminal"
    do script "` + escapeAppleScript(cmd) + `"
    activate
end tell`
	}
}

// shareInvocation builds the `[env KEY=VAL ...] ppz terminal share
// <handle> -- <argv...>` command string used by every --new-window
// backend. envPairs is the agent identity (PPZ_AGENT_HARNESS, etc.) —
// prepended via `env` so the spawned `ppz terminal share` sees the
// pairs in its environment regardless of the user's shell. Empty
// envPairs is a clean no-op for non-agent shares.
func shareInvocation(handle string, envPairs, argv []string) string {
	prefix := ""
	if len(envPairs) > 0 {
		prefix = "env " + strings.Join(envPairs, " ") + " "
	}
	return prefix + "ppz terminal share " + handle + " -- " + strings.Join(argv, " ")
}

// bashSingleQuote wraps s in bash-safe single quotes. Embedded single
// quotes are emitted as `'\''` (close-quote, escaped quote, reopen) —
// the standard pattern for shell-injecting an arbitrary string into a
// command line.
func bashSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// escapeAppleScript quotes a string for embedding inside an AppleScript
// string literal. AppleScript only requires escaping `"` and `\`.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// devNull is an io.Writer that discards everything. Used to silence the
// flag package's default usage output during tests.
type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }

// --- Linux & WSL --new-window support --------------------------------------

// linuxTerminalPriority is the probe order used by selectLinuxTerminal
// when $TERMINAL is unset. Higher-quality emulators come first; bare
// xterm is the penultimate fallback, x-terminal-emulator (Debian
// alternatives) is last so distros that ship it as a wrapper don't
// shadow a real GUI emulator that's also installed.
var linuxTerminalPriority = []string{
	"gnome-terminal", "konsole", "xfce4-terminal", "tilix",
	"wezterm", "kitty", "alacritty", "xterm", "x-terminal-emulator",
}

// selectLinuxTerminal chooses which terminal emulator to drive on Linux.
// If termEnv (typically $TERMINAL) is non-empty it wins; otherwise the
// caller-supplied availability probe is consulted in a fixed priority
// order. Returns ("", error) when no candidate is available.
func selectLinuxTerminal(termEnv string, available func(string) bool) (string, error) {
	if termEnv != "" {
		return termEnv, nil
	}
	for _, name := range linuxTerminalPriority {
		if available(name) {
			return name, nil
		}
	}
	return "", fmt.Errorf("no supported terminal emulator on PATH; tried %v", linuxTerminalPriority)
}

// buildLinuxNewWindowArgv returns the exec argv that opens a new
// <terminal> window running `ppz terminal share <handle> -- <argv...>`.
// cwd, when non-empty, is prepended as `cd '<cwd>' && ` so the spawned
// harness boots in the folder the parent shell was running in
// (matching the macOS path's claude-trust-folder fix).
func buildLinuxNewWindowArgv(terminal, handle, cwd string, envPairs []string, argv []string) ([]string, error) {
	script := shareInvocation(handle, envPairs, argv)
	if cwd != "" {
		script = "cd " + bashSingleQuote(cwd) + " && " + script
	}
	// `-lc` (login + command) makes bash source ~/.profile (and through
	// it ~/.bashrc) before running the script. Plain `-c` is non-login
	// and non-interactive, so it skips the user's PATH additions and
	// the spawned `claude` reads as "executable file not found".
	switch terminal {
	case "gnome-terminal":
		// gnome-terminal swallows trailing argv as its own options unless
		// `--` separates them from the inner command.
		return []string{"gnome-terminal", "--", "bash", "-lc", script}, nil
	case "konsole", "xfce4-terminal", "tilix", "alacritty", "xterm", "x-terminal-emulator":
		return []string{terminal, "-e", "bash", "-lc", script}, nil
	case "kitty":
		// kitty treats trailing argv as the command directly — no flag.
		return []string{"kitty", "bash", "-lc", script}, nil
	case "wezterm":
		return []string{"wezterm", "start", "--", "bash", "-lc", script}, nil
	}
	return nil, fmt.Errorf("unsupported terminal %q (supported: %v)", terminal, linuxTerminalPriority)
}

// isWSL reports whether the calling process is running under Windows
// Subsystem for Linux. The caller passes the contents of /proc/version
// so the function stays unit-testable. Both WSL1 and WSL2 tag their
// kernel string with "microsoft" (case varies between releases).
func isWSL(procVersion string) bool {
	return strings.Contains(strings.ToLower(procVersion), "microsoft")
}

// buildWSLScript returns the bash script body that boots the agent.
// Pure — no side effects. The caller writes the body to a tempfile via
// writeAgentSpawnScript and passes the path to buildWSLNewWindowArgv.
//
// We don't inline this script into the wt.exe argv anymore. wt.exe's
// argv tokenizer corrupts the script in two ways that together break
// every realistic agent prompt:
//
//  1. It treats `;` as a sub-command separator. The default agent
//     prompt's Monitor recipe (`while true; do ... ; sleep 60 ; done`)
//     contains three of them; wt.exe truncates the script at the first
//     one and launches the trailing chunks as standalone Windows
//     programs.
//  2. It collapses `''` (adjacent close-quote / open-quote) sequences,
//     which is exactly the middle of the standard bash single-quote
//     escape `'\''` (used by bashSingleQuote to embed a literal `'`
//     into a single-quoted string). A prompt containing `isn't`
//     becomes `isn'\''t` in the script, which wt.exe collapses to
//     `isn'\t'` — bash then sees an unmatched closing quote and hangs
//     at the PS2 continuation prompt. The "flashing cursor, no source
//     created" symptom on WSL.
//
// Routing the script through a tempfile means wt.exe only sees a
// benign path (no `;`, no `'`), and bash reads the script byte-for-
// byte from disk with its own quoting rules intact.
func buildWSLScript(handle, cwd string, envPairs []string, argv []string) string {
	script := shareInvocation(handle, envPairs, argv)
	if cwd != "" {
		script = "cd " + bashSingleQuote(cwd) + " && " + script
	}
	return script
}

// writeAgentSpawnScript writes the bash script body to a tempfile and
// returns its path. The written script self-cleans (rm -f) on EXIT so
// /tmp doesn't accumulate one-shot helpers. dir is the temp directory
// to use — empty means os.TempDir().
//
// The shebang isn't load-bearing (the script is invoked as `bash -l
// <path>`, which reads commands directly without honoring shebangs)
// but it's set so a user inspecting the leftover file knows what it
// is.
func writeAgentSpawnScript(dir, handle, body string) (string, error) {
	f, err := os.CreateTemp(dir, "ppz-agent-"+handle+"-*.sh")
	if err != nil {
		return "", fmt.Errorf("ppz agent spawn script: %w", err)
	}
	defer f.Close()
	content := "#!/bin/bash\ntrap 'rm -f -- " + bashSingleQuote(f.Name()) + "' EXIT\n" + body + "\n"
	if _, err := f.WriteString(content); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("ppz agent spawn script: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("ppz agent spawn script: %w", err)
	}
	return f.Name(), nil
}

// buildWSLNewWindowArgv returns the exec argv that drives wt.exe
// (Windows Terminal) to open a new tab running `wsl.exe -d <distro>
// bash -l <scriptPath>`. The script must already be on disk — see
// writeAgentSpawnScript and the buildWSLScript comment for why the
// script can't be inlined into the argv.
//
// `-w 0` targets the current Windows Terminal window; `nt` opens a new
// tab inside it. Falls back to a new window if no WT instance exists
// yet. `bash -l <scriptPath>` runs the script as a *login* shell so it
// sources ~/.profile / ~/.bash_profile — most users' PATH additions
// for claude (npm-global, nvm, asdf, ~/.local/bin) live there and a
// plain non-login `bash <path>` would fail to find the binary.
func buildWSLNewWindowArgv(distro, scriptPath string) ([]string, error) {
	if distro == "" {
		return nil, fmt.Errorf("buildWSLNewWindowArgv: empty distro (set $WSL_DISTRO_NAME)")
	}
	if scriptPath == "" {
		return nil, fmt.Errorf("buildWSLNewWindowArgv: empty scriptPath")
	}
	return []string{"wt.exe", "-w", "0", "nt", "wsl.exe", "-d", distro, "bash", "-l", scriptPath}, nil
}
