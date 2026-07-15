package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// callComplete runs handleComplete over an in-memory net.Pipe and
// returns the decoded CompleteReply. Mirrors the callSourceDestroy
// helper in source_destroy_test.go.
func callComplete(t *testing.T, d *Daemon, req cliproto.CompleteRequest) (cliproto.CompleteReply, *cliproto.Error) {
	t.Helper()
	params, _ := json.Marshal(req)
	srvConn, cliConn := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer srvConn.Close()
		d.handleComplete(context.Background(), srvConn, params)
		close(done)
	}()

	var resp ipcResponse
	if err := json.NewDecoder(cliConn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	cliConn.Close()
	<-done

	if resp.Error != nil {
		return cliproto.CompleteReply{}, resp.Error
	}
	var reply cliproto.CompleteReply
	raw, _ := json.Marshal(resp.Result)
	_ = json.Unmarshal(raw, &reply)
	return reply, nil
}

// TestHandleComplete_ServesFromCache: when refreshSourceCache has
// already run, handleComplete returns the cached snapshot without
// hitting the server. This is the steady-state hot path — every tab
// press should land here.
func TestHandleComplete_ServesFromCache(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/sources", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(cliproto.ListSourcesReply{})
	})
	d := newDaemonWithFakeServer(t, mux)

	// Pre-warm the cache the way a real `ppz ls` would.
	d.refreshSourceCache([]cliproto.Source{
		{Handle: "alice", Kind: string(cliproto.KindPTY), Pipes: []string{"alerts"}},
		{Handle: "bob", Kind: string(cliproto.KindMessage)},
	})

	reply, ipcErr := callComplete(t, d, cliproto.CompleteRequest{})
	if ipcErr != nil {
		t.Fatalf("handleComplete: %v", ipcErr)
	}
	if reply.Stale {
		t.Fatalf("reply.Stale = true; cache was pre-warmed so should be fresh")
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("server hit %d times; handleComplete must not call /api/v1/sources when cache is warm", hits)
	}

	got := map[string][]string{}
	for _, s := range reply.Sources {
		sort.Strings(s.Pipes)
		got[s.Handle] = s.Pipes
	}
	// alice (pty) merges pipesForKind(KindPTY) ∪ user pipes; bob (message)
	// gets just pipesForKind(KindMessage). The hardcoded expected sets
	// track the auto-pipe vocabulary defined in handlers.go's pipesForKind
	// — if a new auto-pipe lands (e.g. "stderr"), it must be added to
	// wantAlice (PTY kind) too. Sorted-alpha order matches the input
	// sort.Strings above so the comparison is deterministic.
	wantAlice := []string{"alerts", "heartbeat", "inbox", "stdctrl", "stdin", "stdout", "system"}
	wantBob := []string{"inbox"}
	if !slices.Equal(got["alice"], wantAlice) {
		t.Errorf("alice pipes = %v, want %v", got["alice"], wantAlice)
	}
	if !slices.Equal(got["bob"], wantBob) {
		t.Errorf("bob pipes = %v, want %v", got["bob"], wantBob)
	}
}

// TestHandleComplete_ColdCacheWarmsOnce: on a cold daemon the first
// completion call does a SINGLE cheap GET /api/v1/sources to warm the
// cache. JetStream / /api/v1/pipes must NOT be touched — that's the
// whole reason this verb exists.
func TestHandleComplete_ColdCacheWarmsOnce(t *testing.T) {
	var sourcesHits, pipesHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/sources", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&sourcesHits, 1)
		_ = json.NewEncoder(w).Encode(cliproto.ListSourcesReply{
			Sources: []cliproto.Source{
				{Handle: "carol", Kind: string(cliproto.KindMessage)},
			},
		})
	})
	mux.HandleFunc("GET /api/v1/pipes", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&pipesHits, 1)
		_ = json.NewEncoder(w).Encode(cliproto.ListUncollaredPipesReply{})
	})
	d := newDaemonWithFakeServer(t, mux)

	reply, ipcErr := callComplete(t, d, cliproto.CompleteRequest{})
	if ipcErr != nil {
		t.Fatalf("first handleComplete: %v", ipcErr)
	}
	if len(reply.Sources) != 1 || reply.Sources[0].Handle != "carol" {
		t.Errorf("cold-cache reply = %+v, want carol", reply.Sources)
	}
	if got := atomic.LoadInt32(&sourcesHits); got != 1 {
		t.Errorf("first call: /api/v1/sources hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&pipesHits); got != 0 {
		t.Errorf("first call: /api/v1/pipes hits = %d, want 0 — completion must not enrich uncollared", got)
	}

	// Second call — cache is now warm; no additional server hit.
	if _, ipcErr := callComplete(t, d, cliproto.CompleteRequest{}); ipcErr != nil {
		t.Fatalf("second handleComplete: %v", ipcErr)
	}
	if got := atomic.LoadInt32(&sourcesHits); got != 1 {
		t.Errorf("second call: /api/v1/sources hits = %d, want still 1 (warm cache)", got)
	}
}

// TestHandleComplete_ColdCacheConcurrent: N concurrent tab presses on
// a cold daemon must coalesce into ONE GET /api/v1/sources, not N.
// Without completionWarmMu the cold check-then-act is a classic race
// — every goroutine reads CompletionSnapshot()=false in parallel and
// fires its own probe. The handler gates the first probe with a
// barrier channel so all callers reach the !loaded check before any
// probe completes; this maximises the race window.
func TestHandleComplete_ColdCacheConcurrent(t *testing.T) {
	const N = 8
	var hits int32
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/sources", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release // hold the first probe until we release
		_ = json.NewEncoder(w).Encode(cliproto.ListSourcesReply{
			Sources: []cliproto.Source{{Handle: "carol", Kind: string(cliproto.KindMessage)}},
		})
	})
	d := newDaemonWithFakeServer(t, mux)

	done := make(chan struct{}, N)
	for range N {
		go func() {
			_, _ = callComplete(t, d, cliproto.CompleteRequest{})
			done <- struct{}{}
		}()
	}
	// Give every goroutine time to enter handleComplete and either
	// acquire the warm lock or block on it. 50ms is generous against
	// the in-process pipe + httptest server.
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range N {
		<-done
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("/api/v1/sources hits = %d, want 1 — concurrent tab presses on cold daemon must coalesce", got)
	}
}

// TestHandleComplete_LoggedOutReturnsStale: a daemon without credentials
// returns Stale with no sources and no error. Tab completion must never
// surface an authentication failure mid-keystroke.
func TestHandleComplete_LoggedOutReturnsStale(t *testing.T) {
	home := t.TempDir()
	d := New(home, "") // no SetLogin → no credentials

	reply, ipcErr := callComplete(t, d, cliproto.CompleteRequest{})
	if ipcErr != nil {
		t.Fatalf("handleComplete on logged-out daemon: %v (want clean empty reply)", ipcErr)
	}
	if !reply.Stale {
		t.Errorf("reply.Stale = false; want true on logged-out daemon")
	}
	if len(reply.Sources) != 0 {
		t.Errorf("reply.Sources = %v, want empty on logged-out daemon", reply.Sources)
	}
}
