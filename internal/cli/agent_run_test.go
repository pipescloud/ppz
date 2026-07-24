package cli

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// `agent run` reuses the create flag parser, so all harness/model/prompt
// semantics carry over verbatim. The only parse-level difference is the
// verb woven into the missing-handle usage error, so the user who typed
// `ppz agent run` (not create) sees their own command echoed back.
func TestResolveAgentSpecVerb_RunRequiresHandle(t *testing.T) {
	_, _, err := resolveAgentSpecVerb("run", nil)
	if err == nil {
		t.Fatal("expected error for missing handle")
	}
	if !strings.Contains(err.Error(), "run") {
		t.Errorf("missing-handle error should name the `run` verb so the message matches what the user typed, got: %q", err.Error())
	}
}

// Flag parity: `agent run` resolves the same defaults (claude + opus),
// handle, and prompt as `agent create`.
func TestResolveAgentSpecVerb_RunDefaultsMatchCreate(t *testing.T) {
	spec, handle, err := resolveAgentSpecVerb("run", []string{"ash", "go"})
	if err != nil {
		t.Fatalf("resolveAgentSpecVerb: %v", err)
	}
	if handle != "ash" {
		t.Errorf("handle=%q, want ash", handle)
	}
	if spec.harness != "claude" || spec.model != "opus" {
		t.Errorf("spec=%+v, want harness=claude model=opus", spec)
	}
	if spec.prompt != "go" {
		t.Errorf("prompt=%q, want %q", spec.prompt, "go")
	}
}

// The pre-existing resolveAgentSpec is now a thin wrapper over the verb
// form pinned to "create", so create's missing-handle UX is unchanged.
func TestResolveAgentSpec_MissingHandleStillNamesCreate(t *testing.T) {
	_, _, err := resolveAgentSpec(nil)
	if err == nil {
		t.Fatal("expected error for missing handle")
	}
	if !strings.Contains(err.Error(), "create") {
		t.Errorf("create's missing-handle error must still name `create`, got: %q", err.Error())
	}
}

// Preflight guard: `agent run` runs the harness in the *current* shell.
// If stdin is not a tty (piped, redirected, or a scripted runner) the
// interactive harness would boot into a dead terminal, so reject up
// front rather than launch something that can't be driven.
func TestAgentRunPreflight_RejectsNonTTY(t *testing.T) {
	err := agentRunPreflight(agentSpec{harness: "claude"}, false)
	if err == nil {
		t.Fatal("expected error when stdin is not a tty")
	}
	if !strings.Contains(err.Error(), "tty") && !strings.Contains(err.Error(), "terminal") {
		t.Errorf("non-tty preflight error should mention a tty/terminal so the cause is clear, got: %q", err.Error())
	}
}

// Happy path: a real interactive terminal passes preflight.
func TestAgentRunPreflight_AllowsTTY(t *testing.T) {
	if err := agentRunPreflight(agentSpec{harness: "claude"}, true); err != nil {
		t.Fatalf("preflight with a tty should pass, got: %v", err)
	}
}

// --new-window is a create-only affordance; `agent run` is foreground-
// only in this cut. Reject it explicitly (naming the flag) rather than
// silently ignoring a flag the user clearly meant.
func TestAgentRunPreflight_RejectsNewWindow(t *testing.T) {
	err := agentRunPreflight(agentSpec{harness: "claude", newWindow: true}, true)
	if err == nil {
		t.Fatal("expected error for --new-window on agent run")
	}
	if !strings.Contains(err.Error(), "new-window") {
		t.Errorf("preflight error should name --new-window, got: %q", err.Error())
	}
}

// Provisioning contract — the crux of `agent run` vs `agent create`.
// `agent create` CREATEs a fresh pty source (IPCCreate) and fails with
// E_SOURCE_TAKEN when the handle already exists. `agent run` targets an
// already-set-up agent, so it upgrades in place via the idempotent
// IPCEnsurePTY path and must NEVER call IPCCreate.
func TestRunAgentRun_UsesEnsurePTYNotCreate(t *testing.T) {
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true command not available")
	}
	t.Setenv("PPZ_SESSION", "agent-run-test")

	// A short /tmp dir keeps the unix socket path under macOS's ~104-char
	// sun_path limit (t.TempDir() paths are too long).
	dir, err := os.MkdirTemp("/tmp", "ppz-agent-run-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	requests := serveAgentRunDaemon(t, sock)

	// Drive the ensure-path share directly with `true` as the harness so
	// the wrapped child exits immediately and the share returns.
	if err := terminalShareExisting([]string{"ash", "--", truePath}); err != nil {
		t.Fatalf("terminalShareExisting: %v", err)
	}

	if requests.create.count() != 0 {
		t.Errorf("agent run must not CREATE the source (that's create's job and it fails on an existing handle); got %d IPCCreate call(s)", requests.create.count())
	}
	if requests.ensurePTY.count() != 1 {
		t.Fatalf("agent run should ensure-pty exactly once; got %d", requests.ensurePTY.count())
	}
	for _, got := range requests.ensurePTY.snapshot() {
		if got.Handle != "ash" {
			t.Errorf("ensure-pty targeted %q, want ash", got.Handle)
		}
	}
}

type agentRunRequests struct {
	create    recorder[cliproto.CreateRequest]
	ensurePTY recorder[cliproto.EnsurePTYRequest]
}

func serveAgentRunDaemon(t *testing.T, sock string) *agentRunRequests {
	t.Helper()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen fake daemon: %v", err)
	}
	requests := &agentRunRequests{}
	done := make(chan struct{})
	t.Cleanup(func() { <-done })
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(sock)
	})

	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleAgentRunDaemonConn(conn, requests)
		}
	}()

	return requests
}

func handleAgentRunDaemonConn(conn net.Conn, requests *agentRunRequests) {
	defer conn.Close()
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}

	enc := json.NewEncoder(conn)
	switch req.Method {
	case cliproto.IPCCreate:
		var cr cliproto.CreateRequest
		_ = json.Unmarshal(req.Params, &cr)
		requests.create.add(cr)
		_ = enc.Encode(map[string]any{
			"result": cliproto.CreateReply{Handle: cr.Handle, Subject: "test." + cr.Handle},
		})
	case cliproto.IPCEnsurePTY:
		var ep cliproto.EnsurePTYRequest
		_ = json.Unmarshal(req.Params, &ep)
		requests.ensurePTY.add(ep)
		_ = enc.Encode(map[string]any{
			"result": cliproto.EnsurePTYReply{Handle: ep.Handle, Kind: "pty"},
		})
	case cliproto.IPCSend:
		var br cliproto.SendRequest
		_ = json.Unmarshal(req.Params, &br)
		_ = enc.Encode(map[string]any{
			"result": cliproto.SendReply{
				ID:      "test-id",
				Subject: "test." + br.Handle + "." + br.Channel,
				Bytes:   len(br.Payload),
			},
		})
	case cliproto.IPCRead:
		_, _ = io.Copy(io.Discard, conn)
	}
}
