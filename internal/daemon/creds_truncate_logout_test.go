package daemon

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The e2e harness (tests/lib/reset.sh) logs daemons out between
// scenarios by TRUNCATING the credentials file — documented workaround
// for named-volume mounts where the watcher's stat poll doesn't
// reliably observe a pure unlink. A zero-byte credentials file is
// therefore a deliberate logout signal, exactly like removal — and
// with persistCredsLocked tmp+rename atomic, a crash can never leave
// an empty file at that path by accident. Only NON-empty garbage is
// "corrupt, treat as transient" (the finding-5 hardening). Regressing
// this turned every post-truncate e2e scenario into a logged-in one
// (CI run 32259160000).

// TestLoadFromDisk_TruncatedCredsIsLogout — state level: an empty
// credentials file reloads successfully as logged-out.
func TestLoadFromDisk_TruncatedCredsIsLogout(t *testing.T) {
	st := NewState(t.TempDir())
	if err := st.SetLogin(Credentials{URL: "http://127.0.0.1:1", APIKey: "k1"}, loginTestAcctOld, "alpha", "k1"); err != nil {
		t.Fatalf("SetLogin: %v", err)
	}
	if err := os.Truncate(filepath.Join(st.Home(), fileCredentials), 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := st.LoadFromDisk(); err != nil {
		t.Fatalf("LoadFromDisk on a truncated credentials file must succeed as logout, got %v", err)
	}
	if _, ok := st.Credentials(); ok {
		t.Fatalf("truncated credentials file must read as logged out")
	}
}

// TestWatchState_TruncatedCredsLogsOut — daemon level, the harness's
// exact mechanism: truncate + reload leaves the daemon logged out with
// the who-cache forgotten.
func TestWatchState_TruncatedCredsLogsOut(t *testing.T) {
	d := newLoginTestDaemon(t)
	loginAsPriorAccount(t, d, loginTestAcctOld)
	d.Heartbeats.Stamp("alice", loginTestAcctOld, `{"harness":"claude"}`, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hupCh := make(chan os.Signal, 1)
	go d.watchState(ctx, hupCh)

	if err := os.Truncate(filepath.Join(d.State.Home(), fileCredentials), 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	hupCh <- syscall.SIGHUP

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, ok := d.State.Credentials()
		if !ok && len(d.Heartbeats.Snapshot()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon still logged in after credentials truncation: the e2e harness's " +
		"between-scenario logout (reset.sh truncate) depends on this")
}
