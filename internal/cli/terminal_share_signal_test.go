//go:build linux || darwin

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Reproduces the user-observed "staircase terminal" bug: `ppz terminal
// share` puts the controlling tty into raw mode (MakeRaw clears OPOST, so
// the line discipline stops translating "\n" into "\r\n") and only
// restores it via a deferred term.Restore. Go does NOT run deferred
// functions when a process is killed by an unhandled SIGTERM — the default
// disposition terminates it immediately — so a `kill`-ed share leaves the
// user's shell in raw mode, and every subsequent command's output (ppz
// status, ppz ls, even plain `ls`) staircases down-and-right.
//
// This is inherently a process-level symptom (SIGTERM skipping defers), so
// unlike the other rawmode tests it can't be exercised in-process: we run
// `ppz terminal share` as a REAL child process (a re-exec of this test
// binary) attached to a REAL pty, wait until it has raw'd the tty, send it
// SIGTERM, and then assert the pty was restored to cooked mode (OPOST
// re-set). OPOST is the specific flag whose absence produces the staircase,
// so it's the on-the-nose assertion for this symptom.
//
// Why a Go+pty test and not a tests/ scenario: the docker e2e harness runs
// run.sh with stdin=/dev/null, so term.IsTerminal(stdin) is false and the
// entire raw-mode branch is dead code there (see terminal_rawmode_test.go).
//
// RED until cmdTerminalShare installs a SIGINT/SIGTERM handler that
// restores the terminal before the process exits.
func TestCmdTerminalShareRestoresTerminalOnSIGTERM(t *testing.T) {
	// Child arm: the re-exec lands here with the outer pty slave already
	// wired to our stdio by the parent. Run share wrapping `cat`, which
	// keeps the share alive (never EOFs its stdin on its own) until the
	// parent signals it, then EOFs cleanly when share's inner pty closes.
	if os.Getenv("PPZ_SHARE_SIGNAL_HELPER") == "1" {
		err := cmdTerminalShare([]string{"term-sig", "--", "cat"})
		if err != nil {
			if log := os.Getenv("PPZ_SHARE_SIGNAL_ERRLOG"); log != "" {
				_ = os.WriteFile(log, []byte(err.Error()), 0o644)
			}
		}
		os.Exit(0)
	}

	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not available")
	}

	// Short /tmp path, not t.TempDir(): macOS caps unix socket paths at
	// ~104 chars and the default temp dir blows past that.
	dir, err := os.MkdirTemp("/tmp", "ppz-share-sig-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	serveTerminalShareEchoDaemon(t, sock)

	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	// Precondition: a fresh pty is cooked (OPOST on). If not, the whole
	// assertion is meaningless.
	before, err := termiosForFD(int(slave.Fd()))
	if err != nil {
		t.Fatalf("termiosForFD before: %v", err)
	}
	if before.Oflag&unix.OPOST == 0 {
		t.Fatalf("precondition failed: expected OPOST on for a fresh pty, Oflag=%#x", before.Oflag)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestCmdTerminalShareRestoresTerminalOnSIGTERM$")
	errLog := filepath.Join(dir, "child-err.log")
	cmd.Env = append(os.Environ(),
		"PPZ_SHARE_SIGNAL_HELPER=1",
		"PPZ_IPC_SOCKET="+sock,
		"PPZ_SHARE_SIGNAL_ERRLOG="+errLog,
	)
	dbgR, dbgW, err := os.Pipe()
	if err != nil {
		t.Fatalf("debug pipe: %v", err)
	}
	defer dbgR.Close()
	defer dbgW.Close()
	dbg := newAsyncBuffer(t, dbgR)
	cmd.Stdin = slave
	cmd.Stdout = dbgW
	cmd.Stderr = dbgW
	// Deliberately NOT the session leader / controlling-tty owner: in a real
	// login shell, `ppz terminal share` is just a foreground process whose
	// stdin is the shell's controlling tty — it doesn't own it. Making the
	// child the session leader (Setctty) would also make macOS revoke() the
	// pts when it dies, invalidating the fd we inspect from here.
	if err := cmd.Start(); err != nil {
		t.Fatalf("start share child: %v", err)
	}

	var waitOnce sync.Once
	reap := func() { waitOnce.Do(func() { _ = cmd.Wait() }) }
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		reap()
	})

	// Wait until share has actually put the tty into raw mode (OPOST
	// cleared). Polling the shared pty's termios is how we know MakeRaw ran
	// before we signal — otherwise the post-SIGTERM check would be vacuous.
	rawByDeadline := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		tm, err := termiosForFD(int(slave.Fd()))
		if err == nil && tm.Oflag&unix.OPOST == 0 {
			rawByDeadline = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !rawByDeadline {
		childErr, _ := os.ReadFile(errLog)
		t.Fatalf("share never entered raw mode (OPOST never cleared) — test setup is wrong; child err=%q; child out=%q", childErr, dbg.String())
	}

	// This is the user's `kill` / `zsh: terminated ppz terminal share`.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	// Reap so any deferred- or handler-based restore has fully run.
	reap()

	after, err := termiosForFD(int(slave.Fd()))
	if err != nil {
		t.Fatalf("termiosForFD after: %v (slave fd=%d)", err, slave.Fd())
	}
	if after.Oflag&unix.OPOST == 0 {
		t.Fatalf("terminal left in raw mode after SIGTERM: OPOST still cleared (Oflag=%#x) — this is the staircase bug", after.Oflag)
	}
}
