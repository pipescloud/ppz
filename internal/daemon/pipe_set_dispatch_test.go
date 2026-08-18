package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// handlePipeSet proxies `ppz pipe set` to the server. It makes the same
// collared/uncollared routing decision as handlePipeCreate — request
// shape decides, not a path string — and stamps the session's current
// namespace onto an unset Manifold so `ppz set namespace` applies to
// `pipe set` exactly as it does to `pipe create`.

type pipeSetCapture struct {
	method string
	path   string
	body   cliproto.PipeSetRequest
}

func newPipeSetDaemon(t *testing.T) (*Daemon, *pipeSetCapture) {
	t.Helper()
	capt := &pipeSetCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capt.method = r.Method
		capt.path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &capt.body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cliproto.PipeSetReply{
			Handle: capt.body.Handle, Manifold: capt.body.Manifold, Name: capt.body.Name,
			TTLSeconds: 86400, MaxMsgs: 5000, MaxBytes: 16777216,
		})
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		State:      NewState(t.TempDir()),
		NATSEvents: newNATSEventRing(natsEventRingCap),
		Follows:    newFollowRegistry(),
		Watches:    newWatchRegistry(),
		Heartbeats: NewHeartbeatCache(),
		HTTP:       &http.Client{Timeout: 2 * time.Second},
	}
	if err := d.State.SetLogin(Credentials{URL: srv.URL, APIKey: "ppz_test"}, "", "", ""); err != nil {
		t.Fatalf("SetLogin: %v", err)
	}
	return d, capt
}

func callPipeSet(t *testing.T, d *Daemon, req cliproto.PipeSetRequest) *cliproto.Error {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	params, _ := json.Marshal(req)
	go func() {
		d.handlePipeSet(context.Background(), srvConn, params)
		_ = srvConn.Close()
	}()
	_ = cliConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(cliConn).ReadBytes('\n')
	if err != nil && err != io.EOF {
		t.Fatalf("reading IPC reply: %v", err)
	}
	var resp struct {
		Result cliproto.PipeSetReply `json:"result"`
		Error  *cliproto.Error       `json:"error"`
	}
	_ = json.Unmarshal(line, &resp)
	return resp.Error
}

func TestHandlePipeSet_CollaredRoutesToSourcePath(t *testing.T) {
	d, capt := newPipeSetDaemon(t)
	msgs := 50
	if e := callPipeSet(t, d, cliproto.PipeSetRequest{Handle: "chat", Name: "archive", MaxMsgs: &msgs}); e != nil {
		t.Fatalf("handlePipeSet: %v", e)
	}
	if capt.method != "PATCH" {
		t.Errorf("method = %q, want PATCH", capt.method)
	}
	if capt.path != "/api/v1/sources/chat/pipes/archive" {
		t.Errorf("path = %q, want /api/v1/sources/chat/pipes/archive", capt.path)
	}
}

func TestHandlePipeSet_UncollaredRoutesToPipesPath(t *testing.T) {
	d, capt := newPipeSetDaemon(t)
	msgs := 50
	if e := callPipeSet(t, d, cliproto.PipeSetRequest{Name: "room", MaxMsgs: &msgs}); e != nil {
		t.Fatalf("handlePipeSet: %v", e)
	}
	if capt.method != "PATCH" {
		t.Errorf("method = %q, want PATCH", capt.method)
	}
	if capt.path != "/api/v1/pipes" {
		t.Errorf("path = %q, want /api/v1/pipes (uncollared pipes are body-addressed)", capt.path)
	}
	if capt.body.Name != "room" {
		t.Errorf("body name = %q, want room", capt.body.Name)
	}
}

// `ppz set namespace team1` then `ppz pipe set room --max-msgs=50` must
// address team1.room, not the root-manifold room.
func TestHandlePipeSet_StampsSessionNamespace(t *testing.T) {
	d, capt := newPipeSetDaemon(t)
	if err := d.State.SetNamespace("sess-1", "team1"); err != nil {
		t.Fatalf("SetNamespace: %v", err)
	}
	msgs := 50
	if e := callPipeSet(t, d, cliproto.PipeSetRequest{Name: "room", Session: "sess-1", MaxMsgs: &msgs}); e != nil {
		t.Fatalf("handlePipeSet: %v", e)
	}
	if capt.body.Manifold != "team1" {
		t.Errorf("forwarded manifold = %q, want team1 (stamped from the session namespace)", capt.body.Manifold)
	}
	if capt.body.Session != "" {
		t.Errorf("Session leaked to the server (%q) — it's daemon-side only", capt.body.Session)
	}
}

// Not logged in is a clean error, not a nil-credential panic or a
// request to the empty URL.
func TestHandlePipeSet_NotLoggedIn(t *testing.T) {
	d := &Daemon{
		State:      NewState(t.TempDir()),
		NATSEvents: newNATSEventRing(natsEventRingCap),
		Follows:    newFollowRegistry(),
		Watches:    newWatchRegistry(),
		Heartbeats: NewHeartbeatCache(),
		HTTP:       &http.Client{Timeout: 2 * time.Second},
	}
	msgs := 50
	e := callPipeSet(t, d, cliproto.PipeSetRequest{Name: "room", MaxMsgs: &msgs})
	if e == nil || e.Code != cliproto.ENotLoggedIn {
		t.Fatalf("error = %v, want E_NOT_LOGGED_IN", e)
	}
}
