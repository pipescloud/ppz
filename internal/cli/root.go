// Package cli is the entrypoint for `ppz`. Each verb either talks to the
// daemon over IPC or IS the daemon (`ppz daemon start --foreground`).
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// Run dispatches argv[1:] to the appropriate verb. Returns a *cliproto.Error
// when there is one — main turns that into the standard exit code + stderr.
//
// Verb hierarchy (Phase B):
//
//	ppz daemon {start|stop|login|logout}
//	(source verbs removed in Phase 1 — see ppz terminal/agent create
//	for replacements; current-handle state managed via ppz set/unset)
//	ppz terminal {wrap|watch|peek}     (terminal verbs are reshaped in Phase D)
//	ppz {status|ls|read|send}
//
// Old top-level verbs (`ppz create`, `ppz switch`, `ppz kill`, `ppz login`)
// are removed without aliases — fresh MVP, no users to migrate.
func Run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		os.Exit(2)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "daemon":
		return cmdDaemonGroup(rest)
	case "source":
		return cmdSourceGroup(rest)
	case "pipe":
		return cmdPipeGroup(rest)
	case "acl":
		return cmdACLGroup(rest)
	case "terminal":
		return cmdTerminal(rest)
	case "agent":
		return cmdAgentGroup(rest)
	case "login":
		// Top-level shortcut for `ppz daemon login` — matches the
		// `gh login` / `kubectl login` / `az login` muscle memory.
		return cmdDaemonLogin(rest)
	case "status":
		return cmdStatus(rest)
	case "diagnostics":
		return cmdDiagnostics(rest)
	case "who":
		return cmdWho(rest)
	case "chat", "tui":
		// `tui` is a hidden alias for the original verb name.
		return cmdChat(rest)
	case "set":
		return cmdSet(rest)
	case "unset":
		return cmdUnset(rest)
	case "get":
		return cmdGet(rest)
	case "version":
		return cmdVersion(rest)
	case "upgrade":
		return cmdUpgrade(rest)
	case "ls":
		return cmdLs(rest)
	case "subs":
		return cmdSubsGroup(rest)
	case "schedule":
		return cmdScheduleGroup(rest)
	case "read":
		return cmdRead(rest)
	case "reread":
		return cmdReread(rest)
	case "send":
		return cmdSend(rest)
	case "command":
		return cmdCommand(rest)
	case "completion":
		return cmdCompletion(rest)
	case "__complete":
		// Hidden — invoked by the shell's tab handler. Not in usage.
		return cmdComplete(rest)
	case "-h", "--help":
		usage(os.Stdout)
		return nil
	case "help":
		return cmdHelp(rest)
	}
	fmt.Fprintf(os.Stderr, "ppz: unknown command %q\n", verb)
	usage(os.Stderr)
	os.Exit(2)
	return nil
}

func usage(w *os.File) {
	fmt.Fprintln(w, wrapUsageText(renderTopLevel(), cliproto.TerminalWidth()))
}

// cmdHelp implements `ppz help [<verb-or-topic>...]`. With no argument it
// prints the grouped top-level help. With an argument it prints the detailed
// body for a verb path ("read", "source destroy") or a named topic ("acks",
// "sessions", "globs"). An unknown key prints top-level help to stderr and
// exits 2.
func cmdHelp(args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}
	key := strings.Join(args, " ")
	if _, ok := helpTopics[key]; ok {
		printHelp(os.Stdout, key)
		return nil
	}
	fmt.Fprintf(os.Stderr, "ppz help: unknown topic %q\n", key)
	usage(os.Stderr)
	os.Exit(2)
	return nil
}

// wrapUsageText reflows the static usage block to fit `width` columns.
// It runs two passes:
//
//  1. Normalize: join paragraph lines so the wrap pass can reflow them at
//     the actual terminal width. Two join cases:
//
//     Case A (inline desc): verb line has a 2+ space gap at descCol.
//     Subsequent lines at indent==descCol with no internal gap are pure
//     description prose — join them into the verb line.
//
//     Case B (deep run): consecutive lines at the same deep indent (≥10)
//     with no internal gap are description prose for a verb whose signature
//     was too long to share the line — join them together.
//
//     Sub-items that are indented more than the active descCol/runIndent are
//     left on their own lines so structured blocks (e.g. flag tables) are
//     preserved.
//
//  2. Wrap: each logical line is word-wrapped at width, preserving the
//     verb-signature column and re-indenting continuation runs under the
//     description column.
func wrapUsageText(text string, width int) string {
	if width <= 0 {
		return text
	}

	// findDescCol returns (leadingIndent, descCol) for a line.
	// descCol is the column after the first 2+ space gap past the verb
	// signature, or leadingIndent when no such gap exists.
	findDescCol := func(line string) (leadingIndent, descCol int) {
		for leadingIndent < len(line) && line[leadingIndent] == ' ' {
			leadingIndent++
		}
		descCol = leadingIndent
		for i := leadingIndent; i < len(line); {
			if line[i] != ' ' {
				i++
				continue
			}
			j := i
			for j < len(line) && line[j] == ' ' {
				j++
			}
			if j-i >= 2 {
				descCol = j
				break
			}
			i = j
		}
		return
	}

	// Pass 1: normalize.
	raw := strings.Split(text, "\n")
	joined := make([]string, 0, len(raw))
	pending := ""
	// pendingDescCol and runIndent are mutually exclusive: flushPending resets
	// both, and the switch below sets at most one per new pending line.
	pendingDescCol := 0 // Case A: description column of the current verb line
	runIndent := 0      // Case B: indent shared by a deep description-only run

	flushPending := func() {
		if pending != "" {
			joined = append(joined, pending)
			pending = ""
		}
		pendingDescCol = 0
		runIndent = 0
	}

	for _, line := range raw {
		if line == "" {
			flushPending()
			joined = append(joined, "")
			continue
		}
		indent, dc := findDescCol(line)

		// Case A: continuation at the active description column.
		if pendingDescCol > 0 && indent == pendingDescCol && dc == indent {
			pending += " " + strings.TrimLeft(line, " ")
			continue
		}
		// Case B: continuation within a deep same-indent run.
		if runIndent > 0 && indent == runIndent && dc == indent {
			pending += " " + strings.TrimLeft(line, " ")
			continue
		}

		flushPending()
		pending = line
		switch {
		case dc > indent:
			pendingDescCol = dc
		case indent >= 10 && dc == indent:
			// Threshold of 10 rules out top-level verb lines (indent ≤ 4)
			// and shallow section text, leaving only description-column prose.
			runIndent = indent
		}
	}
	flushPending()

	// Pass 2: word-wrap each logical line.
	out := make([]string, 0, len(joined))
	for _, line := range joined {
		if len([]rune(line)) <= width {
			out = append(out, line)
			continue
		}
		_, descCol := findDescCol(line)
		contentBudget := width - descCol
		if contentBudget <= 4 {
			out = append(out, line)
			continue
		}
		contPrefix := strings.Repeat(" ", descCol)
		firstPrefix := line[:descCol]
		words := strings.Fields(line[descCol:])
		if len(words) == 0 {
			out = append(out, line)
			continue
		}
		var cur strings.Builder
		first := true
		flush := func() {
			if first {
				out = append(out, firstPrefix+cur.String())
				first = false
			} else {
				out = append(out, contPrefix+cur.String())
			}
			cur.Reset()
		}
		for _, word := range words {
			if cur.Len() == 0 {
				cur.WriteString(word)
				continue
			}
			if cur.Len()+1+len([]rune(word)) <= contentBudget {
				cur.WriteByte(' ')
				cur.WriteString(word)
				continue
			}
			flush()
			cur.WriteString(word)
		}
		if cur.Len() > 0 {
			flush()
		}
	}
	return strings.Join(out, "\n")
}

// home + sock resolution. Order: PPZ_IPC_SOCKET env, then $PPZ_HOME/daemon.sock,
// then ~/.ppz/daemon.sock.
func ipcSocket() string {
	if v := os.Getenv("PPZ_IPC_SOCKET"); v != "" {
		return v
	}
	return filepath.Join(home(), "daemon.sock")
}

func home() string {
	if v := os.Getenv("PPZ_HOME"); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".ppz")
	}
	return ".ppz"
}
