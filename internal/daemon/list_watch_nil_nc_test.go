package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// TestBuildFilteredList_NilNC_ReturnsErrorNotPanic pins that
// buildFilteredList degrades gracefully when d.NC is nil instead of
// dereferencing it inside jetstream.New and crashing the whole daemon.
//
// Repro of the CI flake in `terminal/share-inbox-alerts-survives-share-
// daemon-logout`: an in-flight `subs wait` runs subsSnapshot →
// buildFilteredList. subsSnapshot's ensureNATS check passes (NC non-nil),
// but a concurrent `daemon logout` then trips the watchState watcher
// (watcher.go:37), which calls swapNC("watchState-creds-gone", nil) and
// sets d.NC = nil under ncMu. buildFilteredList reads d.NC WITHOUT the
// lock (list_watch.go:136) and passes it to jetstream.New(nil), which
// panics in setReplyPrefix (SIGSEGV). handleConn has no recover(), so the
// panic takes down the daemon process → restart → new PID →
// `daemon_same_pid: no`, and the scenario fails.
//
// Leaving d.NC == nil is exactly the post-swap state the race produces.
// RED: this panics (crashing the test). GREEN: buildFilteredList returns
// E_NATS_UNREACHABLE and the daemon stays up.
func TestBuildFilteredList_NilNC_ReturnsErrorNotPanic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/sources", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(cliproto.ListSourcesReply{
			Sources: []cliproto.Source{{Handle: "foo", Manifold: "team-a"}},
		})
	})
	mux.HandleFunc("GET /api/v1/pipes", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(cliproto.ListUncollaredPipesReply{})
	})
	d := newDaemonWithFakeServer(t, mux)
	d.NC = nil // the state left behind by a concurrent logout's swapNC(nil)

	// A nil-pointer panic here (RED) crashes the test binary; recover so
	// the failure is a clean, attributable t.Fatalf rather than a process
	// abort that masks the rest of the suite.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("buildFilteredList panicked with nil d.NC (want graceful "+
				"E_NATS_UNREACHABLE): %v", r)
		}
	}()

	_, e := d.buildFilteredList(context.Background(), uuid.New(), "test-session", []string{"foo"})
	if e == nil {
		t.Fatalf("buildFilteredList returned nil error with nil d.NC; want E_NATS_UNREACHABLE")
	}
	if e.Code != cliproto.ENATSUnreachable {
		t.Fatalf("buildFilteredList error code = %q, want %q", e.Code, cliproto.ENATSUnreachable)
	}
}
