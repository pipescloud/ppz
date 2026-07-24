package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/daemon"
	"github.com/pipescloud/ppz/internal/harness"
	"github.com/pipescloud/ppz/internal/version"
)

// cmdTerminal dispatches `ppz terminal <subverb>`.
//
//	create H              provision a pty-kind handle H (broadcast + inbox +
//	                      stdin/stdout/stdctrl auto-pipes) and set as the
//	                      session's current handle. No process is run inside
//	                      the pty — for that use `terminal share`.
//	share H [-- CMD ...]  run CMD (or $SHELL) inside a pty bound to H,
//	                      publishing stdout and subscribing to stdin.
//	watch H               follow H.stdout in alt-screen TUI (interactive,
//	                      renders to the local terminal).
//	read H [flags]        wrapper for `read <H>.stdout` with --tty as the
//	                      default output mode. Use this for capturable
//	                      text snapshots (agents, pipes, scripts).
func cmdTerminal(args []string) error {
	if groupHelp("terminal", args) {
		return nil
	}
	if len(args) == 0 {
		printHelp(os.Stderr, "terminal")
		os.Exit(2)
	}
	switch args[0] {
	case "share":
		return cmdTerminalShare(args[1:])
	case "watch":
		return cmdTerminalView(args[1:])
	case "control":
		return cmdTerminalControl(args[1:])
	case "lease":
		return cmdTerminalLease(args[1:])
	case "release":
		return cmdTerminalRelease(args[1:])
	case "read":
		return cmdTerminalRead(args[1:])
	}
	fmt.Fprintf(os.Stderr, "ppz terminal: unknown subcommand %q\n", args[0])
	os.Exit(2)
	return nil
}

// cmdTerminalRead: ppz terminal read <handle> [reread-flags]
//
// Thin wrapper around `ppz reread <handle>.stdout`. Defaults the output
// mode to --tty (vt10x screen render) — the right answer for the agent-
// inspecting-another-agent's-screen use case: rebuild the cumulative
// screen state from every retained byte. Routes through `reread` (the
// forensic verb) so it never advances the caller's cursor — peeking at
// a pty session shouldn't mark its bytes "read" from the perspective of
// some other tool reading the same pipe.
//
// All reread flags (--raw / --json / -l / --limit / --skip / --since) work;
// passing an explicit output-mode flag suppresses the --tty default.
//
// Implementation: build a transformed argv (handle → handle.stdout,
// inject --tty when appropriate) and delegate to cmdReread.
func cmdTerminalRead(args []string) error {
	// Recognise reread's value-flags so we can step over them when finding
	// the positional handle.
	valueFlags := map[string]bool{
		"-l": true, "-limit": true, "--limit": true,
		"-skip": true, "--skip": true, "-since": true, "--since": true,
	}
	modeFlag := func(a string) bool {
		switch a {
		case "-tty", "--tty", "-raw", "--raw", "-json", "--json":
			return true
		}
		return false
	}

	var newArgs []string
	var handle string
	hasMode := false

	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			newArgs = append(newArgs, a)
			if modeFlag(a) || strings.HasPrefix(a, "--tty=") || strings.HasPrefix(a, "--raw=") || strings.HasPrefix(a, "--json=") {
				hasMode = true
			}
			// Consume value-flag's value (next token) so we don't treat
			// it as the positional handle.
			if valueFlags[a] && !strings.Contains(a, "=") && i+1 < len(args) {
				newArgs = append(newArgs, args[i+1])
				i++
			}
			continue
		}
		if handle != "" {
			usageExit("terminal read")
		}
		handle = a
	}

	if handle == "" {
		usageExit("terminal read")
	}
	if strings.Contains(handle, ".") {
		// Don't accept "handle.pipe" — terminal read is .stdout-only.
		return cliproto.New(cliproto.EInvalidHandle)
	}

	// Prepend the transformed target. cmdReread's splitReadArgs handles
	// the positional regardless of position relative to flags.
	newArgs = append([]string{handle + ".stdout"}, newArgs...)

	// Inject --tty unless the user picked a mode explicitly.
	if !hasMode {
		newArgs = append(newArgs, "--tty")
	}

	return cmdReread(newArgs)
}

// cmdTerminalShare: ppz terminal share [<handle>] [-- <cmd> [args...]]
//
// "Share" because the operation is bidirectional: the wrapped pty's stdout
// is published to <handle>.stdout for subscribers to read, AND messages
// published to <handle>.stdin are fed back into the pty as input. So the
// terminal is genuinely shared with whatever consumers attach.
//
// Two invocation shapes:
//
//	ppz terminal share            uses the daemon's current source. Auto-
//	                              creates stdin + stdout pipes on it if
//	                              missing (via the same idempotent path as
//	                              `pipe create`). Errors with
//	                              E_NO_CURRENT_SOURCE if no current is set.
//	ppz terminal share H          creates source H as kind=pty (broadcast +
//	                              stdin + stdout auto-provisioned), then
//	                              shares.
//
// The share loop:
//   - allocates a real PTY,
//   - runs the child command (or $SHELL) inside it,
//   - publishes the PTY master's byte stream to <handle>.stdout,
//   - subscribes to <handle>.stdin and forwards messages to the PTY master,
//   - exports PPZ_CURRENT_HANDLE=<handle> so the child's `ppz broadcast`
//     targets this terminal's source.
//
// Foreground only — blocks until the child exits.
func cmdTerminalShare(args []string) error {
	return terminalShare(args, false)
}

// terminalShareExisting is the entrypoint `ppz agent run` hands off to.
// It behaves like cmdTerminalShare but forces the idempotent ensure-pty
// provisioning path even for an explicit handle: `agent run` targets an
// agent whose source is already set up, so it must upgrade in place
// rather than CREATE a fresh source (which fails with E_SOURCE_TAKEN on
// an existing handle).
func terminalShareExisting(args []string) error {
	return terminalShare(args, true)
}

// terminalShare wraps a child process in a pty and publishes it as the
// source's terminal. ensureExisting selects the provisioning path: when
// true (or when the invocation is bare) the source is upgraded in place
// via IPCEnsurePTY; otherwise a fresh source is CREATEd via IPCCreate.
func terminalShare(args []string, ensureExisting bool) error {
	// Detect bare invocation: no positional handle (either no args, or args
	// start with "--" indicating only a child command was given).
	bare := len(args) == 0 || args[0] == "--"

	var handle string
	var rest []string
	if bare {
		resolved, err := effectiveCurrentHandle()
		if err != nil {
			return err
		}
		handle = resolved
		rest = args
	} else {
		handle = args[0]
		rest = args[1:]
	}
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}

	cmdArgs := rest
	if len(cmdArgs) == 0 {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmdArgs = []string{shell}
	}

	if bare || ensureExisting {
		// Source already exists but may be inbox-only (kind=message, e.g.
		// created via `source create` or `connect`). Sharing a source
		// declares it a terminal, so upgrade it in place: flip kind→pty and
		// provision the FULL pty pipe set (stdin/stdout/stdctrl/system/inbox/
		// heartbeat) via the daemon's trusted ensure-pty path. This is the
		// only way to provision the reserved `system` (write-lease) and
		// `inbox` pipes — the user pipe-create verb refuses reserved names.
		// Idempotent: a no-op when the source is already a pty terminal.
		var reply cliproto.EnsurePTYReply
		if err := daemon.Call(ipcSocket(), cliproto.IPCEnsurePTY,
			cliproto.EnsurePTYRequest{Handle: handle, Session: sessionID()}, &reply); err != nil {
			return err
		}
	} else {
		// Provision a fresh pty source: server-side creates the row + the
		// auto-provisioned streams (broadcast, stdin, stdout).
		// Session is required so the daemon can stamp the manifold from
		// the session's current_namespace (Phase 1.5.1). Without it the
		// source lands at root even when `ppz set namespace X` is active.
		var createReply cliproto.CreateReply
		if err := daemon.Call(ipcSocket(), cliproto.IPCCreate,
			cliproto.CreateRequest{Handle: handle, Kind: string(cliproto.KindPTY), Session: sessionID()},
			&createReply); err != nil {
			return err
		}
	}

	// Allocate PTY ourselves (instead of pty.Start) so we can configure
	// termios on the *slave* before the child inherits it. macOS doesn't
	// honour termios ioctls on the master fd; setting them on the slave
	// is the canonical, cross-platform approach (it's what tmux/screen do).
	ptmx, tty, err := pty.Open()
	if err != nil {
		return fmt.Errorf("pty open: %w", err)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = terminalShareEnv(handle)
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		_ = tty.Close()
		_ = ptmx.Close()
		return fmt.Errorf("pty start: %w", err)
	}
	// Child has its own copy of the slave fd via fork+dup; release ours so
	// EOF on master read fires when the child exits.
	_ = tty.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// If our stdin is a real terminal, switch it to raw mode (so keystrokes
	// pass through unbuffered + un-echoed by the local tty) and propagate
	// resizes via SIGWINCH to the wrapped child's PTY. Both are no-ops
	// when stdin is a pipe/file/devnull (test runners, scripted use).
	//
	// When stdin isn't a tty there's no source size to inherit, so we set
	// a sensible default (80x24) — that way subscribers always see a
	// concrete pty size on .stdctrl, and the wrapped child renders for
	// some real geometry instead of the kernel's 0×0 default.
	// Bind the process stdio to locals for the lifetime of the share.
	// The stdin reader and stdout publisher goroutines below read these;
	// referencing the os.Stdin / os.Stdout globals from a goroutine
	// instead races with tests that reassign them per-invocation
	// (go test -race). Capturing also pins the share's stdio so a later
	// reassignment can't redirect it mid-flight.
	stdin, stdout := os.Stdin, os.Stdout

	// Live screen model for blocked-state detection: the output tee
	// (below) feeds it every byte the child draws, and the resize paths
	// mirror the child PTY's geometry into it so it always renders at
	// the real size. Created before the winsize plumbing so both
	// branches can sync it.
	screen := cliproto.NewLiveScreen(cliproto.DefaultRenderCols, cliproto.DefaultRenderRows)

	stdinIsTTY := term.IsTerminal(int(stdin.Fd()))
	if !stdinIsTTY {
		restorePTYEcho := setPTYInputEcho(ptmx.Fd(), false)
		defer restorePTYEcho()
	}
	if stdinIsTTY {
		oldState, err := term.MakeRaw(int(stdin.Fd()))
		if err == nil {
			restoreTTY := func() { _ = term.Restore(int(stdin.Fd()), oldState) }
			// Clean-exit restore (child exits, we return normally).
			defer restoreTTY()

			// Killing-signal restore. The defer above only runs on a clean
			// return; a process killed by SIGINT/SIGTERM (`kill`, or the
			// `zsh: terminated ppz terminal share` a user sees) takes Go's
			// default disposition, which terminates immediately WITHOUT
			// running defers — leaving the shell's tty in raw mode so every
			// later command's output staircases (OPOST cleared → no
			// "\n"→"\r\n"). We block on cmd.Wait() below, which a signal
			// can't unblock, so we can't rely on returning: catch the signal,
			// restore the tty inline, then re-raise with the default handler
			// so the exit status still reflects the signal. `ppz terminal
			// view` gets this for free via NotifyContext because closing its
			// socket unblocks its loop; share's cmd.Wait() does not, hence the
			// explicit handler. (Ctrl-C never reaches here: raw mode clears
			// ISIG, so the keystroke flows to the wrapped child as a 0x03
			// byte rather than generating SIGINT.) Regression: staircase-tty
			// after kill — see TestCmdTerminalShareRestoresTerminalOnSIGTERM.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			defer func() { signal.Stop(sigCh); close(sigCh) }()
			go func() {
				sig, ok := <-sigCh
				if !ok {
					return // clean exit: channel closed by the defer above.
				}
				restoreTTY()
				signal.Stop(sigCh)
				if s, ok := sig.(syscall.Signal); ok {
					_ = syscall.Kill(os.Getpid(), s)
				}
			}()
		}
		_ = pty.InheritSize(stdin, ptmx)
		publishWinsize(handle, ptmx)
		syncScreenSize(screen, ptmx)

		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		go func() {
			for range winch {
				_ = pty.InheritSize(stdin, ptmx)
				publishWinsize(handle, ptmx)
				syncScreenSize(screen, ptmx)
			}
		}()
	} else {
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: 80, Rows: 24})
		publishWinsize(handle, ptmx)
		syncScreenSize(screen, ptmx)
	}

	var wg sync.WaitGroup

	// Harness detection: identify what's running in the wrapped PTY's
	// foreground and whether it's working or idle, so heartbeats carry
	// live agent info even when the harness was launched by hand rather
	// than via `ppz agent`. The detector is fed by the byte observers
	// wrapped around the stdout/stdin paths below; the poll goroutine
	// re-inspects the foreground process and wakes the heartbeat loop
	// on transitions. See docs/specs/agent-detection.md.
	det := harness.NewDetector(ptyForegroundInspector(ptmx))
	det.SetScreen(func() string { return screen.BottomText(harnessScreenBottomLines) })
	hbWake := make(chan struct{}, 1)
	detTicker := time.NewTicker(detectSnapshotInterval)
	defer detTicker.Stop()
	wg.Add(1)
	go func() {
		defer wg.Done()
		runHarnessDetection(ctx, det, time.Now(), hbWake, detTicker.C)
	}()

	// IdleAfter / Cooldown are tunable via env so e2e tests can drive
	// the alert pump within the harness 30s ceiling. Production
	// defaults (15s idle, 30s cooldown) stand unless explicitly
	// overridden; the env names are intentionally test-flavored.
	// TODO(naming): legacy "INBOX" name preserved across the rename to
	// SubsAlert; the operator-facing names are undocumented and only
	// referenced by the two share-inbox-alerts-survives-* e2e
	// fixtures, so renaming would force a coupled fixture change
	// without benefit. Worth a back-compat-friendly rename pass later.
	idleAfter := envDurationMS("PPZ_TERMINAL_INBOX_IDLE_MS", 15*time.Second)
	cooldown := envDurationMS("PPZ_TERMINAL_INBOX_COOLDOWN_MS", 30*time.Second)
	subsAlerts := newTerminalSubsAlertPumpForPTY(terminalSubsAlertConfig{
		IdleAfter: idleAfter,
		Cooldown:  cooldown,
		Message:   terminalSubsAlertMessage,
		// PPZ_AGENT_HARNESS is exported into this process's env by
		// setAgentEnv (agent.go) when the share is launched via
		// `ppz agent create --<harness>`; standalone `ppz terminal
		// share` invocations have no harness context and fall into
		// the "" arm of submitInputForHarness (plain `\r`).
		Harness: os.Getenv("PPZ_AGENT_HARNESS"),
		// Fire-time confirmation: re-sample the live unread level
		// (fresh snapshot, same IPC `ppz subs ls` uses) immediately
		// before injecting. The subs-wait loop only signals the
		// up-edge — `ppz subs read` advances the cursor without
		// publishing, so the loop's pending bit goes stale with no
		// wakeup to clear it. Sampling the level at the moment of
		// decision is what guarantees no alert fires for an
		// already-read message. daemon.Call (deadline-bounded), not
		// CallWait: a wedged daemon must fail the confirm, not hang
		// the flush loop.
		ConfirmUnread: func() bool {
			var reply cliproto.ListReply
			err := daemon.Call(ipcSocket(), cliproto.IPCSubsList,
				cliproto.SubsListRequest{Session: handle}, &reply)
			return confirmSubsUnreadDecision(reply, err)
		},
		// The input tee makes injected bytes (alert submissions,
		// buffered user-input flushes) taint output causality like
		// local keystrokes — otherwise the alert's echo would read
		// as agent work and flash `ppz who` to working.
	}, ptmx, harnessInputWriter{ptmx, det})

	wg.Add(1)
	go func() {
		defer wg.Done()
		flushSubsAlerts(ctx, subsAlerts)
	}()

	// Local stdin → PTY master with a control-response filter. The local
	// terminal emits DA / cursor-position / focus replies into our stdin
	// in response to queries the wrapped child (or its outer shell) made;
	// passing those through unmodified lets shells with self-inserting
	// readline echo them to the .stdout stream. See filterControlResponses
	// for the matched shapes.
	go func() {
		buf := make([]byte, 1024)
		var pending []byte
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				input := buf[:n]
				if len(pending) > 0 {
					input = append(pending, input...)
					pending = nil
				}
				filtered, p := filterControlResponses(input)
				pending = p
				if len(filtered) > 0 {
					det.ObserveInput(time.Now())
					subsAlerts.ForwardUserInput(time.Now(), filtered)
				}
			}
			if err != nil {
				if len(pending) > 0 {
					det.ObserveInput(time.Now())
					subsAlerts.ForwardUserInput(time.Now(), pending)
				}
				return
			}
		}
	}()

	// PTY master → fan out: (1) local stdout so the user sees the wrapped
	// child like a normal terminal, (2) per-line publisher to <handle>.stdout.
	wg.Add(1)
	go func() {
		defer wg.Done()
		publishAndDisplayStdout(handle, harnessOutputReader{r: ptmx, det: det, screen: screen}, stdout)
	}()

	// Advisory write-lease state, owned by this pty host. The lease manager
	// (below) consumes <handle>.system to grant/release/expire it; forwardStdin
	// reads it to gate which sender's bytes reach the child. Local keystrokes
	// (the stdin→PTY goroutine above) never traverse .stdin, so the lease
	// cannot block the local operator — it only coordinates remote writers.
	lease := newLeaseState()

	// Subscribe to <handle>.stdin → write to PTY master (external `ppz send`
	// reaches the wrapped child via this path). Gated by the lease: while a
	// lease is held, only the holder's bytes pass.
	wg.Add(1)
	go func() {
		defer wg.Done()
		forwardStdin(ctx, handle, harnessInputWriter{ptmx, det}, lease)
	}()

	// Subscribe to <handle>.system → arbitrate the write-lease (acquire /
	// release / TTL expiry) and publish lease-state back to observers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runLeaseManager(ctx, handle, lease)
	}()

	// Subscribe to <handle>.stdctrl → apply viewer-requested resizes to
	// the child PTY. The viewer→wrapper half of the resize channel: a
	// viewer (h2oslide pane, browser) publishes {"type":"setsize",...}
	// and the child is sized to match. Applied directly to the child PTY
	// (not via local SIGWINCH), so it works for headless/remote/container
	// wrappers that have no controlling tty.
	wg.Add(1)
	go func() {
		defer wg.Done()
		forwardResize(ctx, handle, ptmx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		forwardSubsAlerts(ctx, handle, subsAlerts)
	}()

	// Heartbeat ticker: publishes <handle>.heartbeat every
	// HeartbeatIntervalSec seconds with the agent identity (live
	// foreground detection, PPZ_AGENT_* env vars from `ppz agent
	// create` as fallback) plus host/runtime fields. First beat fires
	// immediately so `ppz who` shows a freshly-booted agent without
	// waiting for the first interval; detection state changes wake an
	// immediate out-of-cycle beat the same way. Lives inside the
	// share's ctx so it stops cleanly when the wrapped child exits.
	hbTicker := time.NewTicker(time.Duration(HeartbeatIntervalSec) * time.Second)
	defer hbTicker.Stop()
	wg.Add(1)
	go func() {
		defer wg.Done()
		runHeartbeat(ctx, handle, heartbeatDeps{
			Now:          time.Now,
			Tick:         hbTicker.C,
			Publish:      sendStreamLine,
			GetEnv:       os.Getenv,
			Detect:       func() harness.Detection { return det.Snapshot(time.Now()) },
			StateChanged: hbWake,
			Hostname:     os.Hostname,
			OS:           runtime.GOOS,
			Arch:         runtime.GOARCH,
			PID:          os.Getpid(),
			PPZVersion:   version.Version,
			StartedAt:    time.Now(),
			IntervalSec:  HeartbeatIntervalSec,
		})
	}()

	// Wait for child. Give the kernel + reader a brief window to drain any
	// final bytes still in the PTY buffer; then close the master so the
	// reader's blocking Read returns and goroutines exit.
	_ = cmd.Wait()
	time.Sleep(200 * time.Millisecond)
	_ = ptmx.Close()
	cancel()
	wg.Wait()
	return nil
}

func flushSubsAlerts(ctx context.Context, pump *terminalSubsAlertPump) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			pump.Flush(now)
		}
	}
}

// publishAndDisplayStdout reads the PTY master in chunks. Each chunk is
// fanned out two ways:
//
//	(a) written verbatim to `display` (the user's local stdout) so the
//	    wrapped terminal looks normal,
//	(b) published verbatim to <handle>.stdout — one message per chunk, no
//	    transformation. ANSI escapes survive intact, so `ppz terminal view`
//	    can replay the session in a terminal emulator and `ppz read
//	    <h>.stdout --json` gives byte-faithful access to log consumers.
//
// One read, two consumers — same fan-out as `script(1)` / `screen`.
// Publishing runs on a bounded worker so a slow daemon/NATS round-trip does
// not delay local terminal rendering for typical bursts. The 512-slot bound
// caps pending publish memory at roughly 2 MiB (512 × 4096-byte reads).
func publishAndDisplayStdout(handle string, master io.Reader, display io.Writer) {
	publishCh := make(chan string, 512)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Drain into batches: block for the first message, then sweep
		// up everything else that's already buffered before going back
		// to the daemon. Each batch is one IPCSendBatch — the
		// daemon issues N async publishes plus a single Flush, so a
		// burst of PTY output collapses to ~1 round-trip total instead
		// of N. Without this, under WAN latency the per-call Flush
		// throttles drain to ~5 msg/sec and PTY backpressures the
		// wrapped child.
		const maxBatch = 128
		for {
			first, ok := <-publishCh
			if !ok {
				return
			}
			batch := []string{first}
		drain:
			for len(batch) < maxBatch {
				select {
				case p, ok := <-publishCh:
					if !ok {
						_ = sendStreamBatch(handle, "stdout", batch)
						return
					}
					batch = append(batch, p)
				default:
					break drain
				}
			}
			_ = sendStreamBatch(handle, "stdout", batch)
		}
	}()
	defer func() {
		close(publishCh)
		wg.Wait()
	}()

	buf := make([]byte, 4096)
	var pending []byte // trailing partial UTF-8 sequence carried across reads
	for {
		n, err := master.Read(buf)
		if n > 0 {
			// Display verbatim — local stdout is our user's terminal,
			// which can handle partial bytes correctly because they get
			// completed by the next chunk's bytes arriving immediately.
			_, _ = display.Write(buf[:n])

			// For the publish path, splice carry-over from the previous
			// read in front, then split off any trailing incomplete
			// UTF-8 sequence so JSON marshalling doesn't rewrite
			// truncated bytes as U+FFFD on .stdout consumers.
			merged := buf[:n]
			if len(pending) > 0 {
				merged = append(pending, merged...)
				pending = nil
			}
			complete, partial := splitOnUTF8Boundary(merged)
			pending = partial

			if len(complete) > 0 {
				publishCh <- string(complete)
			}
		}
		if err != nil {
			// Flush any final partial bytes (even if invalid) — better
			// to ship them than drop, parity with legacy behaviour.
			if len(pending) > 0 {
				publishCh <- string(pending)
				pending = nil
			}
			return
		}
	}
}

// sendStreamLine publishes one message to <handle>.<channel> via daemon IPC.
// Errors are swallowed — the publisher loop is best-effort and shouldn't
// abort the whole terminal session if NATS hiccups.
func sendStreamLine(handle, channel, payload string) error {
	var reply cliproto.SendReply
	return daemon.Call(ipcSocket(), cliproto.IPCSend,
		cliproto.SendRequest{
			Handle:  handle,
			Channel: channel,
			Payload: payload,
			// Forward session id so daemon.envelope.sender resolves
			// against this tty's current source — same fix as send.go.
			Session: sessionID(),
		},
		&reply)
}

// sendStreamBatch publishes N messages in one daemon IPC round-trip,
// amortising the per-message NATS Flush across the batch.
func sendStreamBatch(handle, channel string, payloads []string) error {
	if len(payloads) == 0 {
		return nil
	}
	var reply cliproto.SendBatchReply
	return daemon.Call(ipcSocket(), cliproto.IPCSendBatch,
		cliproto.SendBatchRequest{
			Handle:   handle,
			Channel:  channel,
			Payloads: payloads,
			// Forward session id so the daemon resolves
			// envelope.sender = d.State.Current(req.Session) against
			// this tty's current source — same contract as the single-
			// IPCSend path. Without this, a wrapped pty's stdout
			// stream lands sender="" and the receiver can't tell who
			// spoke. Pinned by tests/terminal/terminal-share-uses-
			// current-source.
			Session: sessionID(),
		},
		&reply)
}

// publishWinsize reads the current pty size and publishes a JSON resize
// event to <handle>.stdctrl. Best-effort: a Getsize/publish failure
// shouldn't abort the share. Subscribers (currently the GUI WebSocket
// viewer) read the latest stdctrl message + follow updates to keep
// xterm.js sized to match the source pty — bytes laid out for one width
// can't render right at another.
// terminalShareEnv builds the child process environment for `ppz terminal share`.
// It injects PPZ_CURRENT_HANDLE so ppz verbs default to this source, and
// PPZ_SESSION=<handle> so all subprocesses within the pty (even those that
// call setsid and get a fresh Unix session id) share the same cursor session.
func terminalShareEnv(handle string) []string {
	return append(os.Environ(),
		"PPZ_CURRENT_HANDLE="+handle,
		"PPZ_SESSION="+handle,
	)
}

func publishWinsize(handle string, ptmx *os.File) {
	rows, cols, err := pty.Getsize(ptmx)
	if err != nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"type": "resize",
		"cols": cols,
		"rows": rows,
	})
	if err != nil {
		return
	}
	_ = sendStreamLine(handle, "stdctrl", string(payload))
}

// forwardStdin opens an IPC Read with follow=true on <handle>.stdin and
// pipes every received message into the PTY master verbatim. Callers are
// responsible for including whatever terminator they need (e.g. via
// ppz command which appends \n or --claude etc.).
//
// Resilient to daemon restarts: if the IPC connection drops (daemon
// stop/crash/upgrade), we sleep with backoff and redial. We use
// NoAdvance=true (the daemon's cursor never moves), so on redial the
// daemon redelivers every retained message; we skip ones we've
// already written to the PTY by tracking message IDs in a bounded
// ring. Without dedupe, every reconnect would replay history into
// the wrapped child.
func forwardStdin(ctx context.Context, handle string, master io.Writer, lease *leaseState) {
	seen := newSeenIDRing(1024)

	for {
		if ctx.Err() != nil {
			return
		}
		streamForwardStdinOnce(ctx, handle, master, seen, lease)
		if ctx.Err() != nil {
			return
		}
		// Brief pause before redialling so we don't spin against a
		// daemon that's just been stopped and hasn't been started
		// back up yet.
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func streamForwardStdinOnce(ctx context.Context, handle string, master io.Writer, seen *seenIDRing, lease *leaseState) {
	conn, err := net.Dial("unix", ipcSocket())
	if err != nil {
		return
	}
	defer conn.Close()

	body, _ := json.Marshal(cliproto.ReadRequest{
		Handle:    handle,
		Channel:   "stdin",
		Follow:    true,
		NoAdvance: true,
	})
	if err := json.NewEncoder(conn).Encode(map[string]any{"method": cliproto.IPCRead, "params": json.RawMessage(body)}); err != nil {
		return
	}

	// Close the connection when the parent ctx ends so the daemon stops
	// streaming and Scan() unblocks.
	stopCloser := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopCloser:
		}
	}()
	defer close(stopCloser)

	dec := bufio.NewScanner(conn)
	dec.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for dec.Scan() {
		var evt cliproto.ReadEvent
		if err := json.Unmarshal(dec.Bytes(), &evt); err != nil {
			continue
		}
		if evt.Message == nil {
			continue
		}
		if seen.has(evt.Message.ID) {
			continue
		}
		// Advisory write-lease enforcement: while a lease is held, drop
		// stdin bytes from anyone but the holder. Identity is the message's
		// envelope sender (best-effort / cooperative — see WIRE.md). A
		// dropped message is marked seen so a reconnect replay can't inject
		// it late after the lease has moved on.
		if lease != nil {
			if holder := lease.holderAt(time.Now()); holder != "" && evt.Message.Sender != holder {
				seen.add(evt.Message.ID)
				continue
			}
		}
		_, _ = io.WriteString(master, evt.Message.Payload)
		seen.add(evt.Message.ID)
	}
}

// --- Advisory terminal write-lease (see docs/WIRE.md §terminal control) ---
//
// The lease is arbitrated by the pty host (the `terminal share` process),
// the single serialization point where every .stdin byte enters the child.
// Requests and state travel on <handle>.system as typed JSON:
//
//	{"type":"lease-acquire","ttl_ms":N,"nonce":"..."}                 writer → host
//	{"type":"lease-release","nonce":"..."}                            writer → host
//	{"type":"lease-state","holder":H,"until":T,"reply_nonce":"..."}   host → observers
//
// The holder identity is the acquirer's envelope sender — advisory: it is the
// caller-supplied PPZ_CURRENT_HANDLE, not an authenticated principal, so this
// coordinates cooperating writers rather than defending against hostile ones
// (ACLs are the eventual write-access boundary). The lease is time-bounded so
// a crashed controller self-heals; the holder renews by re-acquiring
// (cur==sender grants) and releases explicitly.
const (
	// defaultLeaseTTL bounds a lease whose acquire carried no positive ttl.
	defaultLeaseTTL = 60 * time.Second
	// leaseAcquireTimeout bounds how long the CLI waits for the host's
	// grant/deny before giving up (host down, .system unprovisioned).
	leaseAcquireTimeout = 5 * time.Second
	// controlLeaseTTL is the window `ppz terminal control` acquires; it
	// renews at controlLeaseRenew while attached so a live session never
	// expires mid-use.
	controlLeaseTTL   = 30 * time.Second
	controlLeaseRenew = 10 * time.Second
	// leaseStaleGrace pads the stale-acquire guard. created_at is emitted at
	// second granularity (truncated), so a just-published acquire can read up
	// to ~1s older than its real publish time; add processing/redelivery slop
	// and clock skew. Without slack a short-TTL acquire (e.g. `lease box 1s`)
	// would be falsely dropped. The churn this guards against is minutes-to-
	// hours-old, so a few seconds of grace costs nothing.
	leaseStaleGrace = 3 * time.Second
)

// leaseState is the pty host's in-memory view of the current write-lease.
// runLeaseManager mutates it; forwardStdin reads it. "" holder = free.
type leaseState struct {
	mu      sync.Mutex
	holder  string
	expires time.Time
}

func newLeaseState() *leaseState { return &leaseState{} }

// holderAt returns the effective holder at `now` — "" when free or expired.
func (l *leaseState) holderAt(now time.Time) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder == "" || !now.Before(l.expires) {
		return ""
	}
	return l.holder
}

func (l *leaseState) expiresAt() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.expires
}

func (l *leaseState) grant(holder string, expires time.Time) {
	l.mu.Lock()
	l.holder, l.expires = holder, expires
	l.mu.Unlock()
}

func (l *leaseState) clear() {
	l.mu.Lock()
	l.holder, l.expires = "", time.Time{}
	l.mu.Unlock()
}

// expireIfDue clears the lease iff it is held but past its expiry, reporting
// whether it did so — so the caller publishes the free state exactly once.
func (l *leaseState) expireIfDue(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != "" && !now.Before(l.expires) {
		l.holder, l.expires = "", time.Time{}
		return true
	}
	return false
}

// leaseMsg is the shared shape of every <handle>.system payload.
type leaseMsg struct {
	Type       string `json:"type"`
	TTLMs      int64  `json:"ttl_ms,omitempty"`
	Holder     string `json:"holder,omitempty"`
	Until      string `json:"until,omitempty"`
	Nonce      string `json:"nonce,omitempty"`
	ReplyNonce string `json:"reply_nonce,omitempty"`
}

// leaseNonce returns a short random token correlating a host reply
// (lease-state.reply_nonce) with the acquire/release that triggered it.
func leaseNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "n"
	}
	return hex.EncodeToString(b[:])
}

// publishLeaseState emits a host→observers lease-state on <handle>.system.
func publishLeaseState(handle, holder string, until time.Time, replyNonce string) {
	m := leaseMsg{Type: "lease-state", Holder: holder, ReplyNonce: replyNonce}
	if !until.IsZero() {
		m.Until = until.UTC().Format(time.RFC3339)
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = sendStreamLine(handle, "system", string(payload))
}

// publishLeaseRequest publishes an acquire/release on <handle>.system stamped
// with `sender` as the caller identity. Returns the daemon error (so an
// unauthenticated caller surfaces ENotLoggedIn → exit 10).
func publishLeaseRequest(handle, sender, msgType, nonce string, ttl time.Duration) error {
	m := leaseMsg{Type: msgType, Nonce: nonce}
	if ttl > 0 {
		m.TTLMs = ttl.Milliseconds()
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var reply cliproto.SendReply
	return daemon.Call(ipcSocket(), cliproto.IPCSend, cliproto.SendRequest{
		Handle:  handle,
		Channel: "system",
		Payload: string(payload),
		Sender:  sender,
		Session: sessionID(),
	}, &reply)
}

// runLeaseManager consumes <handle>.system and arbitrates the write-lease.
// Mirrors forwardResize's resilient redial loop.
func runLeaseManager(ctx context.Context, handle string, lease *leaseState) {
	for {
		if ctx.Err() != nil {
			return
		}
		streamLeaseManagerOnce(ctx, handle, lease)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func streamLeaseManagerOnce(ctx context.Context, handle string, lease *leaseState) {
	conn, err := net.Dial("unix", ipcSocket())
	if err != nil {
		return
	}
	defer conn.Close()

	body, _ := json.Marshal(cliproto.ReadRequest{
		Handle:    handle,
		Channel:   "system",
		Follow:    true,
		NoAdvance: true,
	})
	if err := json.NewEncoder(conn).Encode(map[string]any{"method": cliproto.IPCRead, "params": json.RawMessage(body)}); err != nil {
		return
	}

	stopCloser := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopCloser:
		}
	}()
	defer close(stopCloser)

	// A lease expires on the clock, not on traffic, so we can't block solely
	// on the scanner: a reader goroutine feeds a channel and the main loop
	// selects over {message, expiry timer, ctx}.
	msgCh := make(chan cliproto.ReadMessage, 16)
	go func() {
		dec := bufio.NewScanner(conn)
		dec.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
		for dec.Scan() {
			var evt cliproto.ReadEvent
			if err := json.Unmarshal(dec.Bytes(), &evt); err != nil || evt.Message == nil {
				continue
			}
			select {
			case msgCh <- *evt.Message:
			case <-ctx.Done():
				return
			}
		}
		close(msgCh)
	}()

	expiry := time.NewTimer(time.Hour)
	if !expiry.Stop() {
		<-expiry.C
	}
	defer expiry.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-expiry.C:
			if lease.expireIfDue(time.Now()) {
				publishLeaseState(handle, "", time.Time{}, "")
			}
		case msg, ok := <-msgCh:
			if !ok {
				return // connection ended → redial
			}
			handleLeaseMessage(handle, lease, msg, expiry)
		}
	}
}

// handleLeaseMessage applies one <handle>.system request to the lease and
// publishes the resulting state. Own lease-state echoes and unknown types are
// ignored, so there's no feedback loop.
func handleLeaseMessage(handle string, lease *leaseState, msg cliproto.ReadMessage, expiry *time.Timer) {
	var p leaseMsg
	if err := json.Unmarshal([]byte(msg.Payload), &p); err != nil {
		return
	}
	now := time.Now()
	switch p.Type {
	case "lease-acquire":
		ttl := time.Duration(p.TTLMs) * time.Millisecond
		if ttl <= 0 {
			ttl = defaultLeaseTTL
		}
		// Stale-acquire guard. The lease manager follows .system with
		// NoAdvance, so JetStream redelivers retained acquires (on reconnect
		// or ack-wait expiry). Without this, an acquire that has long outlived
		// its TTL re-grants a phantom lease on every redelivery — generating a
		// perpetual lease-state burst until the message ages out (24h). A live
		// acquire/renewal has age ~0; a redelivered stale one is minutes/hours
		// old. Drop anything older than its TTL plus leaseStaleGrace — the
		// grace covers second-granularity created_at truncation (~1s),
		// redelivery/processing slop, and clock skew, so short-TTL acquires
		// (`lease box 1s`) aren't falsely dropped. An absent/unparseable
		// timestamp fails open (processed) rather than dropping a valid acquire.
		if created, err := time.Parse(time.RFC3339, msg.CreatedAt); err == nil && now.Sub(created) > ttl+leaseStaleGrace {
			return
		}
		cur := lease.holderAt(now)
		if cur == "" || cur == msg.Sender {
			exp := now.Add(ttl)
			lease.grant(msg.Sender, exp)
			armTimer(expiry, ttl)
			publishLeaseState(handle, msg.Sender, exp, p.Nonce)
		} else {
			// Denied — echo the current holder back to the requester so its
			// CLI distinguishes deny from grant via reply_nonce.
			publishLeaseState(handle, cur, lease.expiresAt(), p.Nonce)
		}
	case "lease-release":
		if lease.holderAt(now) == msg.Sender {
			lease.clear()
			stopTimer(expiry)
			publishLeaseState(handle, "", time.Time{}, p.Nonce)
		}
	}
}

// armTimer resets t to fire after d, draining any pending fire first
// (single-consumer, so the drain is race-free).
func armTimer(t *time.Timer, d time.Duration) {
	stopTimer(t)
	t.Reset(d)
}

func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// awaitLeaseGrant follows <handle>.system after an acquire/release and returns
// the holder the host reported for our request (matched by reply_nonce). A
// timeout returns E_LEASE_NO_HOST — the acquire was published but no terminal
// host answered, so the CLI never hangs on an offline/old host. (Distinct from
// EDaemonTimeout: the caller's local daemon is fine; it's the remote host that
// isn't responding.)
func awaitLeaseGrant(handle, nonce string, timeout time.Duration) (string, error) {
	conn, err := net.Dial("unix", ipcSocket())
	if err != nil {
		return "", cliproto.New(cliproto.EDaemonNotRunning)
	}
	defer conn.Close()

	body, _ := json.Marshal(cliproto.ReadRequest{
		Handle:    handle,
		Channel:   "system",
		Follow:    true,
		NoAdvance: true,
		Session:   sessionID(),
	})
	if err := json.NewEncoder(conn).Encode(map[string]any{"method": cliproto.IPCRead, "params": json.RawMessage(body)}); err != nil {
		return "", err
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-time.After(timeout):
			_ = conn.Close()
		case <-done:
		}
	}()

	dec := bufio.NewScanner(conn)
	dec.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	for dec.Scan() {
		var evt cliproto.ReadEvent
		if err := json.Unmarshal(dec.Bytes(), &evt); err != nil || evt.Message == nil {
			continue
		}
		var p leaseMsg
		if err := json.Unmarshal([]byte(evt.Message.Payload), &p); err != nil {
			continue
		}
		if p.Type == "lease-state" && p.ReplyNonce == nonce {
			return p.Holder, nil
		}
	}
	return "", cliproto.NewLeaseNoHost(handle)
}

// cmdTerminalLease: ppz terminal lease <handle> <duration>
//
// Acquires the advisory write-lease on <handle> for <duration> (Go duration:
// 60s, 5m). Blocks until the pty host grants or denies. On grant, exits 0; on
// deny (someone else holds it), prints "held by <holder>" and exits
// E_LEASE_HELD (27). The holder identity is PPZ_CURRENT_HANDLE.
func cmdTerminalLease(args []string) error {
	if len(args) != 2 {
		usageExit("terminal lease")
	}
	handle := args[0]
	if handle == "" || strings.Contains(handle, ".") {
		return cliproto.New(cliproto.EInvalidHandle)
	}
	dur, err := time.ParseDuration(args[1])
	if err != nil || dur <= 0 {
		return fmt.Errorf("ppz terminal lease: invalid duration %q (use e.g. 60s, 5m)", args[1])
	}
	// Identity hint for the acquire: the wrapped-pty env var if present, else
	// empty — in which case the daemon stamps the acquire's sender from the
	// session's current source (same resolution as `ppz send`/`command`).
	// Publishing first also gates login (daemon → ENotLoggedIn, exit 10).
	hint := os.Getenv("PPZ_CURRENT_HANDLE")
	nonce := leaseNonce()
	if err := publishLeaseRequest(handle, hint, "lease-acquire", nonce, dur); err != nil {
		return err
	}
	holder, err := awaitLeaseGrant(handle, nonce, leaseAcquireTimeout)
	if err != nil {
		return err
	}
	// Compare the grant's holder against OUR identity resolved the same way the
	// daemon stamped it (env hint, else session current). Comparing against the
	// raw env var alone misreads a granted lease as "held by <myself>" whenever
	// identity comes from the current source rather than PPZ_CURRENT_HANDLE.
	me, err := effectiveCurrentHandle()
	if err != nil {
		return err
	}
	if holder != me {
		fmt.Fprintf(os.Stderr, "held by %s\n", holder)
		return cliproto.New(cliproto.ELeaseHeld)
	}
	fmt.Fprintf(os.Stderr, "leased %s for %s\n", handle, dur)
	return nil
}

// cmdTerminalRelease: ppz terminal release <handle>
//
// Releases the caller's write-lease on <handle>. A release by anyone other
// than the current holder is a no-op on the host. Blocks briefly for the
// host's confirmation (the free lease-state) so callers can sequence on it.
func cmdTerminalRelease(args []string) error {
	if len(args) != 1 {
		usageExit("terminal release")
	}
	handle := args[0]
	if handle == "" || strings.Contains(handle, ".") {
		return cliproto.New(cliproto.EInvalidHandle)
	}
	sender := os.Getenv("PPZ_CURRENT_HANDLE")
	nonce := leaseNonce()
	if err := publishLeaseRequest(handle, sender, "lease-release", nonce, 0); err != nil {
		return err
	}
	// Best-effort: the host only replies (free state) when the caller was
	// the holder; a non-holder release times out silently, which is fine.
	_, _ = awaitLeaseGrant(handle, nonce, leaseAcquireTimeout)
	fmt.Fprintf(os.Stderr, "released %s\n", handle)
	return nil
}

// cmdTerminalControl: ppz terminal control <handle>
//
// Interactive attach = `terminal watch` (follow .stdout) PLUS forwarding the
// operator's keystrokes to .stdin, after acquiring the write-lease. If another
// controller already holds the lease, control degrades to a read-only attach
// (streams output, keystrokes not forwarded) rather than failing. Exits 10 if
// the daemon isn't logged in.
func cmdTerminalControl(args []string) error {
	if len(args) != 1 {
		usageExit("terminal control")
	}
	handle := args[0]
	if handle == "" || strings.Contains(handle, ".") {
		return cliproto.New(cliproto.EInvalidHandle)
	}
	// Identity hint for the acquire: the wrapped-pty env var if present, else
	// empty (daemon then stamps the sender from the session's current source).
	// Acquire first: surfaces ENotLoggedIn (exit 10) before we attach, and a
	// deny tells us to fall back to read-only.
	hint := os.Getenv("PPZ_CURRENT_HANDLE")
	nonce := leaseNonce()
	if err := publishLeaseRequest(handle, hint, "lease-acquire", nonce, controlLeaseTTL); err != nil {
		return err
	}
	holder, err := awaitLeaseGrant(handle, nonce, leaseAcquireTimeout)
	if err != nil {
		return err
	}
	// Resolve OUR identity the way the daemon stamped the acquire (env hint,
	// else session current) so we compare like-for-like — otherwise a lease
	// granted to our current source reads as "controlled by <ourselves>" and
	// wrongly downgrades to read-only, blocking our own keystrokes.
	me, err := effectiveCurrentHandle()
	if err != nil {
		return err
	}
	writable := holder == me
	if !writable {
		fmt.Fprintf(os.Stderr, "ppz terminal control: %s is controlled by %s — attaching read-only\n", handle, holder)
	}
	return attachTerminal(handle, me, writable)
}

// attachTerminal streams <handle>.stdout to the local terminal and, when
// `writable`, forwards local keystrokes to <handle>.stdin (stamped with
// `sender` so the host's lease check passes) and renews the lease until exit.
// Ctrl-C / Ctrl-D or SIGINT/SIGTERM ends the session; a writable session
// releases the lease on the way out so the terminal frees immediately.
func attachTerminal(handle, sender string, writable bool) error {
	conn, err := net.Dial("unix", ipcSocket())
	if err != nil {
		return cliproto.New(cliproto.EDaemonNotRunning)
	}
	defer conn.Close()

	body, _ := json.Marshal(cliproto.ReadRequest{
		Handle:    handle,
		Channel:   "stdout",
		Follow:    true,
		Session:   sessionID(),
		NoAdvance: true,
	})
	if err := json.NewEncoder(conn).Encode(map[string]any{"method": cliproto.IPCRead, "params": json.RawMessage(body)}); err != nil {
		return err
	}

	stdin, stdout := os.Stdin, os.Stdout
	restoreRaw := setLocalRawMode(stdin.Fd())
	defer restoreRaw()

	_, _ = io.WriteString(stdout, "\x1b[?1049h\x1b[H\x1b[2J")
	defer func() { _, _ = io.WriteString(stdout, "\x1b[?1049l") }()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() { <-ctx.Done(); _ = conn.Close() }()

	// Free the lease promptly on exit rather than waiting out the TTL.
	if writable {
		defer func() { _ = publishLeaseRequest(handle, sender, "lease-release", leaseNonce(), 0) }()
	}

	// Local stdin → forward to .stdin (when writable) + exit on Ctrl-C/D.
	go func() {
		buf := make([]byte, 4096)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			n, rerr := stdin.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				exit := false
				for _, b := range chunk {
					if b == 0x03 || b == 0x04 { // Ctrl-C, Ctrl-D
						exit = true
					}
				}
				if writable {
					publishStdin(handle, sender, chunk)
				}
				if exit {
					cancel()
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Keep the lease alive for the session's duration.
	if writable {
		go func() {
			t := time.NewTicker(controlLeaseRenew)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					_ = publishLeaseRequest(handle, sender, "lease-acquire", leaseNonce(), controlLeaseTTL)
				}
			}
		}()
	}

	dec := bufio.NewScanner(conn)
	dec.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for dec.Scan() {
		var evt cliproto.ReadEvent
		if err := json.Unmarshal(dec.Bytes(), &evt); err != nil {
			continue
		}
		if evt.Error != nil {
			return evt.Error
		}
		if evt.Message != nil {
			_, _ = io.WriteString(stdout, evt.Message.Payload)
		}
	}
	return nil
}

// publishStdin forwards a keystroke chunk to <handle>.stdin stamped with the
// controller's identity so the host's write-lease check accepts it.
func publishStdin(handle, sender string, data []byte) {
	var reply cliproto.SendReply
	_ = daemon.Call(ipcSocket(), cliproto.IPCSend, cliproto.SendRequest{
		Handle:  handle,
		Channel: "stdin",
		Payload: string(data),
		Sender:  sender,
		Session: sessionID(),
	}, &reply)
}

// forwardResize subscribes to <handle>.stdctrl and applies viewer
// "setsize" requests to the child PTY. Mirrors forwardStdin's resilient
// redial loop. It ignores the wrapper's own "resize" messages (which it
// publishes via publishWinsize), so there's no feedback loop; only
// viewer-sent "setsize" messages take effect. Resizing the child PTY
// raises SIGWINCH inside it, so the wrapped program re-renders.
func forwardResize(ctx context.Context, handle string, ptmx *os.File) {
	for {
		if ctx.Err() != nil {
			return
		}
		streamForwardResizeOnce(ctx, handle, ptmx)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func streamForwardResizeOnce(ctx context.Context, handle string, ptmx *os.File) {
	conn, err := net.Dial("unix", ipcSocket())
	if err != nil {
		return
	}
	defer conn.Close()

	body, _ := json.Marshal(cliproto.ReadRequest{
		Handle:    handle,
		Channel:   "stdctrl",
		Follow:    true,
		NoAdvance: true,
	})
	if err := json.NewEncoder(conn).Encode(map[string]any{"method": cliproto.IPCRead, "params": json.RawMessage(body)}); err != nil {
		return
	}

	stopCloser := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopCloser:
		}
	}()
	defer close(stopCloser)

	var last struct{ cols, rows int }
	dec := bufio.NewScanner(conn)
	dec.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	for dec.Scan() {
		var evt cliproto.ReadEvent
		if err := json.Unmarshal(dec.Bytes(), &evt); err != nil || evt.Message == nil {
			continue
		}
		var msg struct {
			Type string `json:"type"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if json.Unmarshal([]byte(evt.Message.Payload), &msg) != nil || msg.Type != "setsize" {
			continue // ignore "resize" (our own) and anything else
		}
		if msg.Cols <= 0 || msg.Rows <= 0 {
			continue
		}
		if msg.Cols == last.cols && msg.Rows == last.rows {
			continue // retained-replay or duplicate; nothing to do
		}
		last.cols, last.rows = msg.Cols, msg.Rows
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(msg.Cols), Rows: uint16(msg.Rows)})
		publishWinsize(handle, ptmx)
	}
}

// seenIDRing keeps a bounded set of message IDs, evicting oldest first.
// Used by forwardStdin to skip retained-message redelivery after a
// daemon-restart redial.
type seenIDRing struct {
	mu    sync.Mutex
	order []string
	idx   int
	set   map[string]struct{}
	cap   int
}

func newSeenIDRing(capacity int) *seenIDRing {
	return &seenIDRing{
		order: make([]string, capacity),
		set:   make(map[string]struct{}, capacity),
		cap:   capacity,
	}
}

func (r *seenIDRing) has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.set[id]
	return ok
}

func (r *seenIDRing) add(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.set[id]; ok {
		return
	}
	if old := r.order[r.idx]; old != "" {
		delete(r.set, old)
	}
	r.order[r.idx] = id
	r.set[id] = struct{}{}
	r.idx = (r.idx + 1) % r.cap
}

// forwardSubsAlerts blocks on `ppz subs wait` repeatedly and feeds
// each level-triggered wakeup with unread rows to the alert pump.
// Resilient to daemon-NC swaps (logout, re-login, refresh-time
// rotation) by the same outer-redial pattern as forwardStdin: on
// any IPC error, fall out of streamForwardSubsAlertsOnce, wait
// 250ms, and reconnect.
//
// Without the wait/redial, a single daemon recycle silently
// disables alerts for the rest of the share session. Pinned by
// share-inbox-alerts-survives-share-daemon-restart.
//
// Why `subs wait` rather than the old IPC Follow over the inbox
// channel: the pump only ever cared about "something subscribed is
// unread", and the old Follow path bound rigidly to <handle>.inbox.
// `subs wait` blocks on the per-session subscription set as a
// whole, so a message landing on a subscribed room (any pipe the
// agent added via `ppz subs add`) fires the alert too. Pinned by
// share-subs-alerts-fire-for-subscribed-room.
//
// The pump's `pending` flag + cooldown handle de-dup; we no longer
// need a seenIDRing here because subs wait returns row state, not
// per-message envelopes, and the state machine already coalesces
// repeated "unread" observations into a single alert per cooldown
// window. forwardStdin still uses seenIDRing for its own
// per-message Follow path; the type stays.
func forwardSubsAlerts(ctx context.Context, handle string, pump *terminalSubsAlertPump) {
	for {
		if ctx.Err() != nil {
			return
		}
		streamForwardSubsAlertsOnce(ctx, handle, pump)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// streamForwardSubsAlertsOnce runs the SubsWait/observe loop on a
// single IPC connection until it errors (daemon stop, NC swap), at
// which point it returns so the outer redial loop can reconnect.
//
// Session: handle — auto-subs are keyed under the handle at
// handleCreate time, and the wrapped agent's own `subs add` calls
// inherit PPZ_SESSION=handle from the share env so they live under
// the same key. The pump runs in the parent process before any
// child sets PPZ_SESSION, so we pass the session id explicitly.
//
// Each successful wakeup with `subsReplyHasUnread(reply)` true
// flips the pump's pending bit; the 250ms throttle past the
// observe call keeps us from spinning against a daemon that keeps
// returning level-triggered "still unread" until the wrapped agent
// runs `ppz subs read` and clears the cursor. False-positive empty
// wakeups (a documented `subs wait` behaviour) are ignored.
//
// This loop only ever sees the UP-edge: after the agent reads, the
// next SubsWait blocks (a cursor advance publishes nothing, so no
// wakeup fires) — there is no reliable down-edge to observe here.
// Stale pending bits are instead neutralised at fire time by the
// pump's ConfirmUnread gate (see the pump construction in
// cmdTerminalShare), which re-samples the live unread level before
// injecting.
func streamForwardSubsAlertsOnce(ctx context.Context, handle string, pump *terminalSubsAlertPump) {
	for {
		if ctx.Err() != nil {
			return
		}
		var reply cliproto.ListReply
		// CallWaitCtx (not CallWait) so the in-flight blocking SubsWait
		// unblocks when the share's ctx is cancelled. Without ctx
		// cancellation here, cmd.Wait()→cancel()→wg.Wait() in
		// cmdTerminalShare blocks forever because this goroutine is
		// stuck inside a deadline-less IPC Decode — the share process
		// never exits and every terminal-related e2e fixture that
		// expects clean shutdown (`wait_for "! kill -0 $SHARE_PID"`)
		// times out at 30s.
		if err := daemon.CallWaitCtx(ctx, ipcSocket(), cliproto.IPCSubsWait,
			cliproto.SubsWaitRequest{Session: handle}, &reply); err != nil {
			return
		}
		if subsReplyHasUnread(reply) {
			pump.ObserveSubsUnread(time.Now())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// subsReplyHasUnread reports whether any row in a ListReply returned
// by IPCSubsWait carries unread > 0. Guards against the documented
// false-positive empty wakeup (subs wait can return with no rows,
// exit 0; observing that as "unread" would re-fire an alert the
// agent has already actioned). Mirrors the row-walk inside
// `cmd subs read` / `cmd subs ls`.
func subsReplyHasUnread(reply cliproto.ListReply) bool {
	for _, s := range reply.Sources {
		for _, p := range s.PipeInfos {
			if p.Unread > 0 {
				return true
			}
		}
	}
	for _, u := range reply.UncollaredPipes {
		if u.Info.Unread > 0 {
			return true
		}
	}
	return false
}

// cmdTerminalView: ppz terminal watch <handle>
// (function name preserved for git-history continuity; the verb is `watch`).
//
// A proper TUI viewer for the wrapped pty's .stdout channel. On startup it
// enters the alternate screen (\x1b[?1049h) so the user's existing scroll-
// back is preserved; while running it drains stdin so any escape responses
// the local terminal emits (DA1, focus, cursor-position) are absorbed
// rather than leaking to the post-view shell session; on Ctrl-C / SIGTERM
// it restores the alternate screen and exits cleanly.
//
// Trade-offs vs an `attach`-style viewer (deferred):
//   - User keystrokes during view are discarded (drained to /dev/null).
//     If you want to type into the wrapped session, that's a separate
//     `terminal attach` mode, not built yet.
//   - No grid emulation: viewers with different terminal sizes from the
//     source still render at the source's geometry. Mid-session join
//     replays whatever's in JetStream retention; your local terminal
//     converges on the right state but you may see flicker.
//   - No SIGWINCH propagation back to the source.
func cmdTerminalView(args []string) error {
	if len(args) != 1 {
		usageExit("terminal watch")
	}
	handle := args[0]
	if handle == "" || strings.Contains(handle, ".") {
		// view takes a bare handle — channel is implicit (.stdout).
		return cliproto.New(cliproto.EInvalidHandle)
	}

	conn, err := net.Dial("unix", ipcSocket())
	if err != nil {
		return cliproto.New(cliproto.EDaemonNotRunning)
	}
	defer conn.Close()

	body, _ := json.Marshal(cliproto.ReadRequest{
		Handle:    handle,
		Channel:   "stdout",
		Follow:    true,
		Session:   sessionID(),
		NoAdvance: true,
	})
	if err := json.NewEncoder(conn).Encode(map[string]any{
		"method": cliproto.IPCRead,
		"params": json.RawMessage(body),
	}); err != nil {
		return err
	}

	// Bind the process stdio to locals, same as cmdTerminalShare: the
	// stdin-drain goroutine below reads os.Stdin, and referencing the
	// global from a goroutine races with tests that reassign it. No test
	// exercises cmdTerminalView today, but capturing keeps the pattern
	// from biting the first one that does.
	stdin, stdout := os.Stdin, os.Stdout

	// Put the local tty in raw mode so user keystrokes aren't echoed
	// locally by the terminal's line discipline. Drained below — but the
	// echo is the emulator's job, only stoppable by clearing ECHO. No-op
	// when stdin isn't a tty (test runner, scripted use). Tested by
	// TestSetLocalRawMode_*.
	restoreRaw := setLocalRawMode(stdin.Fd())
	defer restoreRaw()

	// Enter alt screen; ensure we exit it no matter how we leave this
	// function (normal completion, error, panic). Sequence:
	//   \x1b[?1049h  enter alt screen, save state
	//   \x1b[H       cursor home
	//   \x1b[2J      erase screen
	//   ...payload bytes flow here...
	//   \x1b[?1049l  exit alt screen, restore previous content + cursor
	_, _ = io.WriteString(stdout, "\x1b[?1049h\x1b[H\x1b[2J")
	defer func() {
		_, _ = io.WriteString(stdout, "\x1b[?1049l")
	}()

	// SIGINT / SIGTERM → close socket → daemon stops sending → we drain
	// any remaining events and exit. The deferred alt-screen-exit and
	// raw-mode-restore then run.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() { <-ctx.Done(); _ = conn.Close() }()

	// Drain stdin while we're alive. In raw mode Ctrl-C arrives as byte
	// 0x03 (not a signal) so we look for it explicitly and trigger the
	// same exit path as SIGINT. All other bytes are silently consumed —
	// view is read-only by design (see WIRE.md notes; an `attach` mode
	// that forwards keystrokes is a separate verb).
	go func() {
		buf := make([]byte, 256)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			n, err := stdin.Read(buf)
			if err != nil {
				return
			}
			for i := 0; i < n; i++ {
				switch buf[i] {
				case 0x03, 0x04: // Ctrl-C, Ctrl-D
					cancel()
					return
				}
			}
		}
	}()

	dec := bufio.NewScanner(conn)
	dec.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for dec.Scan() {
		var evt cliproto.ReadEvent
		if err := json.Unmarshal(dec.Bytes(), &evt); err != nil {
			continue
		}
		if evt.Error != nil {
			return evt.Error
		}
		if evt.Message != nil {
			_, _ = io.WriteString(stdout, evt.Message.Payload)
		}
	}
	return nil
}

// envDurationMS reads an integer-millisecond env var, returning the
// fallback if unset or unparseable. Negative values fall back too.
func envDurationMS(name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return time.Duration(n) * time.Millisecond
}

// silence unused import errors when we trim things out during dev.
var _ = errors.New
