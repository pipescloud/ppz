package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/daemon"
)

// `ppz completion <bash|zsh>` emits a shell script the user sources from
// their rc file. The script registers a completion function that calls
// the hidden `ppz __complete` subcommand on every tab. The subcommand
// receives the partial word list and prints one candidate per line.
//
// Two-stage design (script ↔ __complete) keeps the heavy lifting in Go
// where we already have access to verb metadata + the daemon's pipe
// list. The shell script stays tiny and stable; rules can change in
// `__complete` without users re-sourcing.
//
// Setup (printed by `cmdCompletion` itself):
//
//	# bash:
//	eval "$(ppz completion bash)"
//	# zsh:
//	eval "$(ppz completion zsh)"
//
// Add to ~/.bashrc / ~/.zshrc to persist.

const completionBash = `# ppz bash completion
_ppz_complete() {
    local words=("${COMP_WORDS[@]:1:$COMP_CWORD}")
    local IFS=$'\n'
    COMPREPLY=( $(ppz __complete -- "${words[@]}" 2>/dev/null) )
    return 0
}
complete -F _ppz_complete ppz
`

const completionZsh = `# ppz zsh completion
_ppz() {
    local -a completions
    local words=("${(@)words[2,$CURRENT]}")
    completions=("${(@f)$(ppz __complete -- "${words[@]}" 2>/dev/null)}")
    compadd -- $completions
}
compdef _ppz ppz
`

// cmdCompletion handles `ppz completion <shell>`.
func cmdCompletion(args []string) error {
	if wantsHelp(args) {
		printHelp(os.Stdout, "completion")
		return nil
	}
	if len(args) != 1 {
		usageExit("completion")
	}
	switch args[0] {
	case "bash":
		fmt.Print(completionBash)
	case "zsh":
		fmt.Print(completionZsh)
	default:
		fmt.Fprintf(os.Stderr, "ppz completion: unsupported shell %q (bash, zsh)\n", args[0])
		os.Exit(2)
	}
	return nil
}

// Top-level verbs offered by the completion engine. Order is for the
// shell to display when the user hits tab on `ppz <tab>` — keep
// alphabetical so it scans easily; "completion" / "__complete" are
// intentionally omitted (operator-internal, not for everyday use).
//
// Add every new top-level verb here. TestComplete_TopLevel_IncludesNewVerbs
// guards against regressions — adding a `case "foo":` in root.go without
// touching this list fails CI.
var topLevelVerbs = []string{
	"agent",
	"chat",
	"command",
	"daemon",
	"diagnostics",
	"get",
	"login",
	"ls",
	"pipe",
	"acl",
	"read",
	"reread",
	"schedule",
	"send",
	"set",
	"source",
	"status",
	"subs",
	"terminal",
	"unset",
	"upgrade",
	"version",
	"who",
}

// subverbs maps a grouped top-level verb to its second-positional
// completions. A verb absent from this map either takes no subverb
// (e.g. status, ls, version) or takes only flags (e.g. diagnostics,
// who) — in both cases the dispatcher falls through to the
// target/handle logic or quietly emits nothing, which is the desired
// behaviour for flag-only verbs.
var subverbs = map[string][]string{
	"agent":    {"create"},
	"daemon":   {"start", "stop", "restart", "login", "logout"},
	"pipe":     {"create", "set", "destroy", "acl"},
	"acl":      {"ls", "whoami"},
	"source":   {"create", "destroy"},
	"schedule": {"ls", "rm"},
	"subs":     {"ls", "add", "rm", "wait", "read"},
	"terminal": {"share", "watch", "read"},
	"set":      {"handle", "namespace"},
	"unset":    {"handle", "namespace"},
	"get":      {"handle"},
}

// Verbs that take a `<handle>.<pipe>` target as their first positional.
// Tab on the target slot completes against everything the daemon's
// IPCList knows about.
var targetTakingVerbs = map[string]bool{
	"read":   true,
	"reread": true,
	"send":   true,
}

// Terminal subverbs whose first positional is a handle (no `.pipe`).
// `share` creates a new pty source — completing existing handles is
// still useful as a "remember what I called the other one" hint.
var terminalHandleSubverbs = map[string]bool{
	"share": true,
	"watch": true,
	"read":  true,
}

// pipeSubverbsTakingTargets: subverbs of `ppz pipe` whose first
// positional is an existing <handle>.<pipe> — destroy and set. `create`
// is excluded (it names a NEW pipe, so there's no existing target to
// complete against).
var pipeSubverbsTakingTargets = map[string]bool{
	"destroy": true,
	"set":     true,
}

// sourceSubverbsTakingPatterns: subverbs of `ppz source` whose first
// positional is a glob pattern over both handles AND <handle>.<pipe>
// (see usage doc — bare matches sources, dotted matches pipes). The
// completion offers both vocabularies so the user can pick a real name
// before adding glob chars.
var sourceSubverbsTakingPatterns = map[string]bool{
	"destroy": true,
}

// subsSubverbsTakingTargets: subverbs of `ppz subs` that accept a
// repeating <target> argument list. `add`/`rm` per the user-confirmed
// design (#1 — same vocabulary as send/read). `wait`/`read`/`ls` take
// no positionals.
var subsSubverbsTakingTargets = map[string]bool{
	"add": true,
	"rm":  true,
}

// cmdComplete is the hidden completion engine. The shell script invokes
// it as `ppz __complete -- <word1> <word2> …` where the LAST word is
// the partial currently being typed (may be empty). Earlier words give
// context.
//
// Output: one candidate per line, no formatting, no errors on stdout.
// Failures (e.g. daemon down for dynamic completion) silently fall back
// to nothing rather than spamming the user's tab key with errors.
func cmdComplete(args []string) error {
	// Strip a leading "--" if the shell script left it in argv. We use
	// `--` in the script so words starting with `-` aren't mis-parsed
	// as flags — but Go's args slice has already split on it.
	for len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		// No word at all — e.g. "ppz" then immediately tab. Treat as
		// completing an empty top-level verb.
		args = []string{""}
	}
	cur := args[len(args)-1]
	prior := args[:len(args)-1]

	// Position 1: top-level verb.
	if len(prior) == 0 {
		emitMatching(topLevelVerbs, cur)
		return nil
	}

	verb := prior[0]

	// Position 2: subverb for grouped verbs.
	if subs, ok := subverbs[verb]; ok && len(prior) == 1 {
		emitMatching(subs, cur)
		return nil
	}

	// Target-taking verbs: complete `<handle>.<pipe>` from daemon.
	if targetTakingVerbs[verb] && len(prior) == 1 {
		emitTargets(cur)
		return nil
	}

	// `terminal {share|watch|read} <handle>` — complete handles.
	if verb == "terminal" && len(prior) == 2 && terminalHandleSubverbs[prior[1]] {
		emitHandles(cur)
		return nil
	}

	// `command <handle>` — complete handles.
	if verb == "command" && len(prior) == 1 {
		emitHandles(cur)
		return nil
	}

	// `pipe destroy <target>` — single target slot.
	if verb == "pipe" && len(prior) == 2 && pipeSubverbsTakingTargets[prior[1]] {
		emitTargets(cur)
		return nil
	}

	// `source destroy <pattern>` — patterns over handles AND
	// handle.pipe (usage doc, locked decision #21). Emit both
	// vocabularies — user can extend with glob chars after picking.
	if verb == "source" && len(prior) == 2 && sourceSubverbsTakingPatterns[prior[1]] {
		emitHandlesAndTargets(cur)
		return nil
	}

	// `subs {add|rm} <target>...` — variadic. Every positional after
	// the subverb is a target slot (the daemon dedupes, so suggesting
	// already-typed targets again is harmless and matches send/read).
	if verb == "subs" && len(prior) >= 2 && subsSubverbsTakingTargets[prior[1]] {
		emitTargets(cur)
		return nil
	}

	// Anything else — no completion. Quietly succeed.
	return nil
}

// emitMatching prints each candidate that prefix-matches `cur`.
func emitMatching(candidates []string, cur string) {
	for _, c := range candidates {
		if strings.HasPrefix(c, cur) {
			fmt.Println(c)
		}
	}
}

// listSourcesForCompletion is a package-level seam — tests overwrite
// it with a canned snapshot. Production points it at the daemon (see
// listSourcesForCompletionLive). Errors are swallowed: completion
// must never fail loudly, and "daemon's down so suggest nothing" is
// the right fallback.
var listSourcesForCompletion = listSourcesForCompletionLive

// listSourcesForCompletionLive prefers IPCComplete (the lean cache-
// served verb) over IPCList for the tab hot path. IPCList does two
// HTTP RTTs and N JetStream calls per pipe just to learn names — too
// slow to run on every keystroke. IPCComplete answers from the
// daemon's in-memory cache.
//
// We fall back to IPCList on E_PROTOCOL "unknown method" so a newer
// CLI talking to a not-yet-restarted older daemon still completes
// (slowly) instead of going silent.
func listSourcesForCompletionLive() []cliproto.Source {
	var cr cliproto.CompleteReply
	err := daemon.Call(ipcSocket(), cliproto.IPCComplete,
		cliproto.CompleteRequest{Session: sessionID()}, &cr)
	if err == nil {
		return completeReplyToSources(cr)
	}
	// Pre-IPCComplete daemons return E_PROTOCOL "unknown method".
	// Fall back to the heavy verb so completion still works during
	// rolling restarts. Any other error (daemon down, timeout) gives
	// up silently.
	if e, ok := err.(*cliproto.Error); ok && e.Code == "E_PROTOCOL" {
		var lr cliproto.ListReply
		if err := daemon.Call(ipcSocket(), cliproto.IPCList,
			cliproto.ListRequest{Session: sessionID()}, &lr); err != nil {
			return nil
		}
		return lr.Sources
	}
	return nil
}

// completeReplyToSources reshapes the lean CompleteReply into the
// []cliproto.Source the emit* helpers already consume. The reverse
// adapter keeps callers identical regardless of which daemon verb
// answered.
//
// cr.Stale is intentionally discarded — from the shell's point of
// view "daemon hasn't populated its cache yet" and "daemon has no
// sources" both render the same: zero suggestions. Surfacing
// staleness to the user mid-keystroke would be noise.
func completeReplyToSources(cr cliproto.CompleteReply) []cliproto.Source {
	out := make([]cliproto.Source, 0, len(cr.Sources))
	for _, s := range cr.Sources {
		src := cliproto.Source{Handle: s.Handle}
		for _, p := range s.Pipes {
			src.PipeInfos = append(src.PipeInfos, cliproto.PipeInfo{Pipe: p})
		}
		out = append(out, src)
	}
	return out
}

// emitHandles prints handles from the daemon's known sources.
func emitHandles(cur string) {
	seen := map[string]bool{}
	for _, s := range listSourcesForCompletion() {
		if seen[s.Handle] {
			continue
		}
		seen[s.Handle] = true
		if strings.HasPrefix(s.Handle, cur) {
			fmt.Println(s.Handle)
		}
	}
}

// emitTargets prints `<handle>.<pipe>` for every (source, pipe) the
// daemon knows about.
func emitTargets(cur string) {
	for _, s := range listSourcesForCompletion() {
		for _, p := range s.PipeInfos {
			t := s.Handle + "." + p.Pipe
			if strings.HasPrefix(t, cur) {
				fmt.Println(t)
			}
		}
	}
}

// emitHandlesAndTargets prints both bare handles AND <handle>.<pipe>
// — used for `source destroy <pattern>` where both forms are valid
// (bare → match sources, dotted → match pipes). Deduped on handle so
// each handle appears once; pipes are listed in their daemon order.
func emitHandlesAndTargets(cur string) {
	seen := map[string]bool{}
	for _, s := range listSourcesForCompletion() {
		if !seen[s.Handle] {
			seen[s.Handle] = true
			if strings.HasPrefix(s.Handle, cur) {
				fmt.Println(s.Handle)
			}
		}
		for _, p := range s.PipeInfos {
			t := s.Handle + "." + p.Pipe
			if strings.HasPrefix(t, cur) {
				fmt.Println(t)
			}
		}
	}
}
