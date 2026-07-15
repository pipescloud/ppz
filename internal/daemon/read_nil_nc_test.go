package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// TestHandleRead_NilNC_ReturnsErrorNotPanic pins that the read/follow path
// degrades gracefully when d.NC is nil instead of dereferencing it inside
// jetstream.New(d.NC) at read.go:109 and panicking.
//
// This is the same TOCTOU shape as the buildFilteredList flake, on a hotter
// path: handleRead's ensureNATS (read.go:79) passes, but d.NC can be nil at
// the unlocked jetstream.New read at line 109 — either because a concurrent
// `daemon logout` swapNC(nil)'d it, or (deterministically reproduced here)
// because /auth/exchange returned an empty NATS URL, so rebuildNC never
// dialed and left d.NC nil while ensureNATS still returned success.
//
// RED: handleRead panics at read.go:109. GREEN: it returns
// E_NATS_UNREACHABLE — the same fail-soft error every other JetStream site
// returns — and the daemon stays up.
func TestHandleRead_NilNC_ReturnsErrorNotPanic(t *testing.T) {
	orgID := "00000000-0000-0000-0000-000000000001"

	// Fake server whose /auth/exchange returns an empty NATS URL: ensureNATS
	// → bootstrapNATS sets d.NATSURL="", rebuildNC early-returns nil (nothing
	// to dial), and d.NC stays nil while ensureNATS reports success — exactly
	// the state the logout race leaves behind at the jetstream.New call.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/exchange", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(cliproto.AuthExchangeReply{
			NATSURL:     "", // <- no URL: NC never gets dialed
			AccountID:   orgID,
			AccountName: "alpha",
			ExpiresAt:   time.Now().Add(time.Hour),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	d := New(t.TempDir(), "")
	if err := d.State.SetLogin(Credentials{
		URL:       srv.URL,
		APIKey:    "pz_test",
		AccountID: orgID,
	}, orgID, "alpha", "pz_test"); err != nil {
		t.Fatalf("SetLogin: %v", err)
	}
	t.Cleanup(func() {
		if d.Refresh != nil {
			d.Refresh.Stop()
		}
	})

	// Uncollared (BareTarget) read so handleRead skips the source-verify HTTP
	// call and reaches ensureNATS → jetstream.New directly.
	ev, panicked := callRead(t, d, cliproto.ReadRequest{
		BareTarget: "lobby",
		Session:    "test-session",
	})
	if panicked != nil {
		t.Fatalf("handleRead panicked with nil d.NC (want graceful "+
			"E_NATS_UNREACHABLE): %v", panicked)
	}
	if ev.Error == nil {
		t.Fatalf("handleRead returned no error with nil d.NC; want E_NATS_UNREACHABLE")
	}
	if ev.Error.Code != cliproto.ENATSUnreachable {
		t.Fatalf("handleRead error code = %q, want %q", ev.Error.Code, cliproto.ENATSUnreachable)
	}
}

// callRead drives d.handleRead over an in-memory pipe (mirroring
// callSourceDestroy) and returns the first ReadEvent written, plus any
// panic the handler raised (nil if it returned normally). Driving handleRead
// directly — not via handleConn — means handleConn's recover() does NOT mask
// a panic here, so the RED state surfaces as a caught panic rather than a
// silently-swallowed E_INTERNAL.
func callRead(t *testing.T, d *Daemon, req cliproto.ReadRequest) (cliproto.ReadEvent, any) {
	t.Helper()
	params, _ := json.Marshal(req)
	srvConn, cliConn := net.Pipe()

	panicCh := make(chan any, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				panicCh <- r
			}
			srvConn.Close()
		}()
		d.handleRead(context.Background(), srvConn, params)
	}()

	var ev cliproto.ReadEvent
	decErr := json.NewDecoder(cliConn).Decode(&ev)
	cliConn.Close()
	<-done

	select {
	case p := <-panicCh:
		return cliproto.ReadEvent{}, p
	default:
	}
	if decErr != nil {
		t.Fatalf("decode read event: %v", decErr)
	}
	return ev, nil
}
