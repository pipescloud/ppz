package cli

import (
	"flag"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/daemon"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// cmdPipeGroup dispatches `ppz pipe <subverb>` to create / destroy.
func cmdPipeGroup(args []string) error {
	if groupHelp("pipe", args) {
		return nil
	}
	if len(args) == 0 {
		printHelp(os.Stderr, "pipe")
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		return cmdPipeCreate(args[1:])
	case "set":
		return cmdPipeSet(args[1:])
	case "destroy":
		return cmdPipeDestroy(args[1:])
	}
	fmt.Fprintf(os.Stderr, "ppz pipe: unknown subcommand %q\n", args[0])
	os.Exit(2)
	return nil
}

// cmdPipeCreate parses `ppz pipe create [<handle>.]<name> [--ttl=DUR] [--max-msgs=N] [--max-bytes=B]`.
//
// Bare `<name>` uses the current source from daemon state (resolved daemon-
// side via the `Handle` field on the request being empty — the daemon then
// fills it from State.Current).
//
// `<handle>.<name>` is the explicit form.
//
// `--ttl` accepts a Go duration string (24h, 168h, 30m, …).
// `--max-bytes` accepts plain ints, or sizes like "64MiB" / "1GB".
func cmdPipeCreate(args []string) error {
	target, flagArgs := splitTargetAndFlags(args)
	fs := flag.NewFlagSet("pipe create", flag.ExitOnError)
	ttl := fs.Duration("ttl", 0, "stream MaxAge override (e.g. 24h, 168h)")
	maxMsgs := fs.Int("max-msgs", 0, "stream MaxMsgs override")
	maxBytesS := fs.String("max-bytes", "", "stream MaxBytes override (int, or 1KB/1MB/1GiB/...)")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if target == "" {
		usageExit("pipe create")
	}

	handle, name, err := splitHandleName(target)
	if err != nil {
		return cliproto.New(cliproto.EInvalidPipe)
	}
	// Phase 1.5.1: bare LEAF always creates uncollared at the current
	// namespace. Current handle plays no role in create destination —
	// that's a sender-identity concept used only by sends. To create
	// a collared pipe, user types the explicit dotted form HANDLE.LEAF.

	req := cliproto.PipeCreateRequest{Handle: handle, Name: name, Session: sessionID()}
	if *ttl > 0 {
		secs := int(*ttl / time.Second)
		req.TTLSeconds = &secs
	}
	if *maxMsgs > 0 {
		req.MaxMsgs = maxMsgs
	}
	if *maxBytesS != "" {
		b, err := parseBytes(*maxBytesS)
		if err != nil {
			return cliproto.New(cliproto.EInvalidPipe)
		}
		req.MaxBytes = &b
	}

	var reply cliproto.PipeCreateReply
	if err := daemon.Call(ipcSocket(), cliproto.IPCPipeCreate, req, &reply); err != nil {
		return err
	}
	cliproto.PrintPipeCreate(os.Stdout, reply)
	return nil
}

// cmdPipeSet parses `ppz pipe set [<handle>.]<name> [--ttl=DUR] [--max-msgs=N] [--max-bytes=B]`.
//
// Changes the retention of an EXISTING pipe. Target grammar and flag
// names are identical to `pipe create` — one retention vocabulary, not
// two — so a bare LEAF addresses an uncollared pipe at the session's
// current namespace (Phase 1.5.1: current handle is sender identity, not
// destination routing) and HANDLE.LEAF is the explicit collared form.
//
// Only the flags the user actually typed travel on the wire; the rest
// stay nil so the server merges onto the stored row instead of resetting
// untouched fields to their defaults.
//
// Unlike create, this reaches reserved auto-pipes (inbox, stdout) — they
// are exactly the pipes whose default caps bite first, and create can
// never name them.
func cmdPipeSet(args []string) error {
	target, flagArgs := splitTargetAndFlags(args)
	fs := flag.NewFlagSet("pipe set", flag.ExitOnError)
	ttl := fs.Duration("ttl", 0, "stream MaxAge override (e.g. 24h, 168h)")
	maxMsgs := fs.Int("max-msgs", 0, "stream MaxMsgs override")
	maxBytesS := fs.String("max-bytes", "", "stream MaxBytes override (int, or 1KB/1MB/1GiB/...)")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if target == "" {
		usageExit("pipe set")
	}

	handle, name, err := splitHandleName(target)
	if err != nil {
		return cliproto.New(cliproto.EInvalidPipe)
	}

	req := cliproto.PipeSetRequest{Handle: handle, Name: name, Session: sessionID()}
	if *ttl > 0 {
		secs := int(*ttl / time.Second)
		req.TTLSeconds = &secs
	}
	if *maxMsgs > 0 {
		req.MaxMsgs = maxMsgs
	}
	if *maxBytesS != "" {
		b, err := parseBytes(*maxBytesS)
		if err != nil {
			return cliproto.New(cliproto.EInvalidPipe)
		}
		req.MaxBytes = &b
	}
	// No flag named nothing to change. Fail here rather than round-trip
	// to the server to be told nothing happened.
	if req.TTLSeconds == nil && req.MaxMsgs == nil && req.MaxBytes == nil {
		return &cliproto.Error{Code: cliproto.EInvalidPipe,
			Message: "pipe set: name at least one of --ttl, --max-msgs, --max-bytes"}
	}

	var reply cliproto.PipeSetReply
	if err := daemon.Call(ipcSocket(), cliproto.IPCPipeSet, req, &reply); err != nil {
		return err
	}
	cliproto.PrintPipeSet(os.Stdout, reply)
	return nil
}

// cmdPipeDestroy: `ppz pipe destroy [<handle>.]<name>` or
// `ppz pipe destroy --recursive HANDLE`.
//
// The --recursive form destroys every pipe under the handle (and the
// handle's underlying source row). Replaces `ppz source destroy HANDLE`
// from pre-Phase 1 (locked decision #21). Without --recursive, the
// target must be a single pipe name — passing a bare handle would
// previously have routed through the source-destroy glob; that path
// is gone, so a bare handle without --recursive errors out.
func cmdPipeDestroy(args []string) error {
	fs := flag.NewFlagSet("pipe destroy", flag.ExitOnError)
	recursive := fs.Bool("recursive", false, "destroy all pipes under HANDLE (and the handle itself)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		usageExit("pipe destroy")
	}

	if *recursive {
		// Bare handle (no dot). Use IPCSourceDestroy to clear all
		// pipes under it. The IPC verb name retains "source" for
		// now — the user-facing surface is the --recursive flag.
		var reply cliproto.SourceDestroyReply
		if err := daemon.Call(ipcSocket(), cliproto.IPCSourceDestroy,
			cliproto.SourceDestroyRequest{Handle: rest[0]}, &reply); err != nil {
			return err
		}
		cliproto.PrintSourceDestroy(os.Stdout, reply)
		return nil
	}

	// Phase 1.5.1: bare LEAF always destroys uncollared at the
	// current namespace (symmetric with create — current_handle
	// plays no role in destination routing). Dotted form is the
	// explicit collared destroy.
	target := rest[0]

	// Glob form (any of *, ?, [): expand against the user's pipe set
	// and destroy each match. Skips auto-pipes (inbox/stdin/stdout/
	// stdctrl/broadcast) so sources stay intact — those belong to
	// `ppz source destroy`.
	if strings.ContainsAny(target, "*?[") {
		return pipeDestroyGlob(target)
	}

	var handle, name, bareTarget string
	if !strings.Contains(target, ".") {
		bareTarget = target
	} else {
		h, n, err := splitHandleName(target)
		if err != nil {
			return cliproto.New(cliproto.EInvalidPipe)
		}
		handle, name = h, n
	}

	var reply cliproto.PipeDestroyReply
	if err := daemon.Call(ipcSocket(), cliproto.IPCPipeDestroy,
		cliproto.PipeDestroyRequest{Handle: handle, Name: name, BareTarget: bareTarget, Session: sessionID()}, &reply); err != nil {
		return err
	}
	cliproto.PrintPipeDestroy(os.Stdout, reply)
	return nil
}

// pipeDestroyGlob implements `ppz pipe destroy PATTERN` where PATTERN
// contains glob wildcards (* ? [).
//
// Semantics, mirroring `ppz source destroy` glob but scoped to
// user-created pipes only (auto-pipes are left alone so sources
// stay alive):
//
//   - bare pattern (no dot): matches uncollared pipe names AND
//     names of user-created pipes attached to any source. So
//     `pipe destroy '*'` deletes every user-created pipe across the
//     account.
//   - dotted pattern (handle.pipe): matches collared user-pipes only.
//     Handle and pipe parts are independently globbed.
//
// Errors per match follow rm(1): each failure goes to stderr, the
// loop continues, and the command exits non-zero if any destroy
// failed. Order: collared first (alphabetical by handle.pipe), then
// uncollared (alphabetical by name) — gives deterministic test
// output and matches the source-destroy convention.
func pipeDestroyGlob(pattern string) error {
	var listReply cliproto.ListReply
	if err := daemon.Call(ipcSocket(), cliproto.IPCList,
		cliproto.ListRequest{Session: sessionID()}, &listReply); err != nil {
		return err
	}

	collared, uncollared, err := resolvePipeGlob(pattern, listReply.Sources, listReply.UncollaredPipes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ppz pipe destroy: invalid pattern %q: %v\n", pattern, err)
		os.Exit(2)
	}

	if len(collared) == 0 && len(uncollared) == 0 {
		fmt.Fprintln(os.Stdout, "0 pipes destroyed")
		return nil
	}

	hadErr := false
	for _, req := range collared {
		var reply cliproto.PipeDestroyReply
		if err := daemon.Call(ipcSocket(), cliproto.IPCPipeDestroy, req, &reply); err != nil {
			if e, ok := err.(*cliproto.Error); ok {
				fmt.Fprintf(os.Stderr, "error: %s: %s\n", e.Code, e.Message)
			} else {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
			hadErr = true
			continue
		}
		cliproto.PrintPipeDestroy(os.Stdout, reply)
	}
	for _, req := range uncollared {
		var reply cliproto.PipeDestroyReply
		if err := daemon.Call(ipcSocket(), cliproto.IPCPipeDestroy, req, &reply); err != nil {
			if e, ok := err.(*cliproto.Error); ok {
				fmt.Fprintf(os.Stderr, "error: %s: %s\n", e.Code, e.Message)
			} else {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
			hadErr = true
			continue
		}
		cliproto.PrintPipeDestroy(os.Stdout, reply)
	}
	if hadErr {
		os.Exit(1)
	}
	return nil
}

// resolvePipeGlob matches a glob pattern against the user's pipe set
// and returns two ordered destroy-request lists: collared (handle.pipe
// targets) and uncollared (bare-name targets). Auto-pipes are skipped.
func resolvePipeGlob(pattern string, sources []cliproto.Source, uncollared []cliproto.UncollaredPipe) ([]cliproto.PipeDestroyRequest, []cliproto.PipeDestroyRequest, error) {
	dotted := strings.Contains(pattern, ".")
	var handlePat, namePat string
	if dotted {
		h, n, err := splitHandleName(pattern)
		if err != nil {
			return nil, nil, err
		}
		handlePat, namePat = h, n
	} else {
		namePat = pattern
	}

	var collared, uncollaredOut []cliproto.PipeDestroyRequest
	for _, s := range sources {
		// Dotted patterns constrain on handle; bare patterns match
		// any source's user-pipes.
		if dotted {
			matched, err := path.Match(handlePat, s.Handle)
			if err != nil {
				return nil, nil, err
			}
			if !matched {
				continue
			}
		}
		for _, p := range s.PipeInfos {
			if natsubj.AutoProvisionedPipes[p.Pipe] {
				continue
			}
			matched, err := path.Match(namePat, p.Pipe)
			if err != nil {
				return nil, nil, err
			}
			if matched {
				collared = append(collared, cliproto.PipeDestroyRequest{Handle: s.Handle, Name: p.Pipe})
			}
		}
	}
	// Uncollared pipes are only candidates for bare patterns —
	// the dotted form (handle.pipe) requires a handle.
	if !dotted {
		for _, p := range uncollared {
			matched, err := path.Match(namePat, p.Name)
			if err != nil {
				return nil, nil, err
			}
			if matched {
				uncollaredOut = append(uncollaredOut, cliproto.PipeDestroyRequest{
					BareTarget: p.Name,
					Manifold:   p.Manifold,
					Session:    sessionID(),
				})
			}
		}
	}
	return collared, uncollaredOut, nil
}

// splitTargetAndFlags lets the user write the target before OR after flags.
// Returns the first positional and the remaining args (which are flag-only).
func splitTargetAndFlags(args []string) (target string, flagArgs []string) {
	valueFlags := map[string]bool{
		"-ttl": true, "--ttl": true,
		"-max-msgs": true, "--max-msgs": true,
		"-max-bytes": true, "--max-bytes": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if strings.Contains(a, "=") || !valueFlags[a] {
				continue
			}
			if i+1 < len(args) {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
			continue
		}
		if target == "" {
			target = a
		}
	}
	return target, flagArgs
}

// splitHandleName parses "name" or "handle.name". Returns ("", name, nil)
// for bare names; ("handle", "name", nil) for the dotted form. An empty
// component on either side is an error.
func splitHandleName(s string) (handle, name string, err error) {
	idx := strings.LastIndex(s, ".")
	if idx < 0 {
		if s == "" {
			return "", "", fmt.Errorf("empty target")
		}
		return "", s, nil
	}
	if idx == 0 || idx == len(s)-1 {
		return "", "", fmt.Errorf("empty side of dotted target")
	}
	return s[:idx], s[idx+1:], nil
}

// parseBytes accepts a plain int (literal byte count) or one of the common
// SI/IEC suffixes. Case-insensitive on the suffix.
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	// Find the split between digits and the suffix.
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("no leading digits")
	}
	num, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0, err
	}
	suffix := strings.ToLower(strings.TrimSpace(s[i:]))
	switch suffix {
	case "", "b":
		return num, nil
	case "k", "kb":
		return num * 1000, nil
	case "ki", "kib":
		return num * 1024, nil
	case "m", "mb":
		return num * 1000 * 1000, nil
	case "mi", "mib":
		return num * 1024 * 1024, nil
	case "g", "gb":
		return num * 1000 * 1000 * 1000, nil
	case "gi", "gib":
		return num * 1024 * 1024 * 1024, nil
	}
	return 0, fmt.Errorf("unknown size suffix %q", suffix)
}
