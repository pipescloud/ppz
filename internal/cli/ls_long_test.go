package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// lsLongSocket points the CLI at a throwaway unix socket and returns it,
// so each subtest gets its own fake daemon. Pairs with the package's
// existing serveLsListDaemon rather than standing up a second IPCList
// fake beside it.
func lsLongSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ppz-ls-long-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)
	return sock
}

// `-l` / `--long` is the Linux `ls -l` spelling: same rows, extra detail
// columns. Go's flag package treats a single and double dash as the same
// flag, so registering "l" and "long" as aliases covers `-l`, `--l`,
// `-long` and `--long`.

// The flag must reach the daemon, because that is where retention is
// read from — the daemon already holds each pipe's *jetstream.StreamInfo
// for the counts it renders, and Config carries MaxAge/MaxMsgs/MaxBytes.
// Gating population there (rather than stripping at render) is what
// keeps `ls --json` byte-identical when -l is absent.
func TestCmdLs_LongFlagReachesDaemon(t *testing.T) {
	for _, spelling := range []string{"-l", "--long", "-long"} {
		t.Run(spelling, func(t *testing.T) {
			t.Setenv("PPZ_SESSION", "ls-long-test")
			sock := lsLongSocket(t)
			requests := serveLsListDaemon(t, sock)

			if err := cmdLs([]string{spelling}); err != nil {
				t.Fatalf("cmdLs %s: %v", spelling, err)
			}
			if requests.lists.count() != 1 {
				t.Fatalf("list request count = %d, want 1", requests.lists.count())
			}
			if !requests.lists.at(0).Long {
				t.Errorf("cmdLs %s did not set Long on the ListRequest — the daemon would never populate retention", spelling)
			}
		})
	}
}

// Absent the flag, Long must stay false: this is the guard on the
// promise that default `ls` and `ls --json` are unchanged.
func TestCmdLs_WithoutLongLeavesRequestUnchanged(t *testing.T) {
	t.Setenv("PPZ_SESSION", "ls-long-test")
	sock := lsLongSocket(t)
	requests := serveLsListDaemon(t, sock)

	if err := cmdLs(nil); err != nil {
		t.Fatalf("cmdLs: %v", err)
	}
	if requests.lists.at(0).Long {
		t.Error("plain `ppz ls` set Long — default output must not change")
	}
}

// -l composes with --json rather than conflicting with it: the user
// asked for the detail, the format flag says how to render it.
func TestCmdLs_LongComposesWithJSON(t *testing.T) {
	t.Setenv("PPZ_SESSION", "ls-long-test")
	sock := lsLongSocket(t)
	requests := serveLsListDaemon(t, sock)

	if err := cmdLs([]string{"--json", "-l"}); err != nil {
		t.Fatalf("cmdLs --json -l: %v", err)
	}
	if !requests.lists.at(0).Long {
		t.Error("`ls --json -l` did not request Long")
	}
}

// The pre-separation loop in cmdLs splits flags from positional patterns
// so ordering is free; -l must survive it in either position.
func TestCmdLs_LongSurvivesPatternOrdering(t *testing.T) {
	for _, args := range [][]string{{"-l", "chat*"}, {"chat*", "-l"}} {
		t.Setenv("PPZ_SESSION", "ls-long-test")
		sock := lsLongSocket(t)
		requests := serveLsListDaemon(t, sock)

		if err := cmdLs(args); err != nil {
			t.Fatalf("cmdLs %v: %v", args, err)
		}
		got := requests.lists.at(0)
		if !got.Long {
			t.Errorf("cmdLs %v did not set Long", args)
		}
		if len(got.Patterns) != 1 || got.Patterns[0] != "chat*" {
			t.Errorf("cmdLs %v patterns = %v, want [chat*]", args, got.Patterns)
		}
	}
}

// GAP CLOSED: --watch takes a DIFFERENT request type, and nothing tied
// the two together. Dropping `Long: *long` from the ListWatchRequest
// left every test green, because cmdLs renders using its own local
// *long regardless of what came back — so `ls --watch -l` would print
// the long headers over three columns of "-", which reads as "this pipe
// has no caps" rather than "nobody asked for the data".
//
// --watch is the documented wake-signal primitive for agent monitor
// loops, so that is a bad place for a silently-wrong answer.
func TestCmdLs_LongReachesTheWatchRequestToo(t *testing.T) {
	t.Setenv("PPZ_SESSION", "ls-long-test")
	sock := lsLongSocket(t)
	requests := serveLsListDaemon(t, sock)

	if err := cmdLs([]string{"--watch", "-l"}); err != nil {
		t.Fatalf("cmdLs --watch -l: %v", err)
	}
	if requests.watches.count() != 1 {
		t.Fatalf("ListWatch request count = %d, want 1", requests.watches.count())
	}
	if !requests.watches.at(0).Long {
		t.Error("`ls --watch -l` did not set Long on the ListWatchRequest — the CLI would render long headers the daemon never filled")
	}
	// And the snapshot preflight carries it too: cmdLs issues both.
	if !requests.lists.at(0).Long {
		t.Error("`ls --watch -l` did not set Long on the snapshot ListRequest")
	}
}

// The negative half, mirroring the snapshot guard: no flag, no Long.
func TestCmdLs_WatchWithoutLongLeavesRequestUnchanged(t *testing.T) {
	t.Setenv("PPZ_SESSION", "ls-long-test")
	sock := lsLongSocket(t)
	requests := serveLsListDaemon(t, sock)

	if err := cmdLs([]string{"--watch"}); err != nil {
		t.Fatalf("cmdLs --watch: %v", err)
	}
	if requests.watches.at(0).Long {
		t.Error("plain `ls --watch` set Long — default output must not change")
	}
}
