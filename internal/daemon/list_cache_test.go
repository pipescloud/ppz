package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// `ppz ls PATTERN` (and `ppz ls --watch PATTERN`) must populate the
// daemon's handle→manifold cache the same way bare `ppz ls` does.
// Otherwise pattern-ls-as-first-daemon-interaction leaves
// HandleManifold empty for namespaced sources; downstream paths that
// read the cache without self-healing — handleRead's stream-name
// build at read.go:106, ack-emit publishEnvelope at publish.go:130 —
// then resolve to the root manifold, miss the actual stream, and
// return E_PIPE_NOT_FOUND.
//
// Repro shape:
//
//	1. Daemon restarts (cache cold).
//	2. `ppz ls foo%` → buildFilteredList runs, sources fetched.
//	3. `ppz read foo.inbox` on a namespaced foo → wrong manifold.
//
// resolveSendTarget self-heals (KnowsPipe miss triggers refresh +
// ResetSources at handlers.go:1078), so send escapes this trap.
// handleRead does not. Fix scope: one ResetSources call inside
// buildFilteredList, which closes both the new ls-PATTERN path and
// the pre-existing --watch path in one move.
func TestBuildFilteredList_RefreshesHandleManifoldCache(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/sources", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(cliproto.ListSourcesReply{
			Sources: []cliproto.Source{
				{Handle: "foo", Manifold: "team-a"},
			},
		})
	})
	mux.HandleFunc("GET /api/v1/pipes", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(cliproto.ListUncollaredPipesReply{})
	})
	d := newDaemonWithFakeServer(t, mux)
	d.NC = startEmbeddedJS(t)

	if got := d.State.HandleManifold("foo"); got != "" {
		t.Fatalf("pre-state: HandleManifold(foo) = %q, want empty (cold start)", got)
	}

	if _, e := d.buildFilteredList(context.Background(), uuid.New(), "test-session", []string{"foo"}, false); e != nil {
		t.Fatalf("buildFilteredList: %v", e)
	}

	if got := d.State.HandleManifold("foo"); got != "team-a" {
		t.Fatalf("post buildFilteredList: HandleManifold(foo) = %q, want %q — "+
			"pattern-ls left cache cold; a follow-up `ppz read foo.inbox` would "+
			"misroute to root and return E_PIPE_NOT_FOUND", got, "team-a")
	}
}
