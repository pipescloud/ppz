package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/version"
)

// IPC wire format (per WIRE.md §7): newline-delimited JSON. One req per
// connection, one resp, then close. Subscribe streams envelopes until close.
//
// Request:  {"method":"<Name>","params":<obj>}
// Response: {"result":<obj>}     OR     {"error":{"code":"E_*","message":"..."}}

type ipcRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type ipcResponse struct {
	Result any             `json:"result,omitempty"`
	Error  *cliproto.Error `json:"error,omitempty"`
}

func (d *Daemon) serveIPC(ctx context.Context, ln net.Listener) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		go d.handleConn(ctx, conn)
	}
}

func (d *Daemon) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// A panic in any handler must not take down the whole daemon: one
	// connection's bug — e.g. a nil-conn deref racing a logout NC swap
	// (the share-inbox-logout flake) — would otherwise SIGSEGV the process
	// and orphan every other live session. Recover per-connection, log the
	// stack to stderr (→ daemon.log) so it stays diagnosable, and hand this
	// one client a generic internal error. Registered after conn.Close so
	// it runs first (LIFO) and can still write the reply before the close.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "panic in handleConn: %v\n%s\n", r, debug.Stack())
			writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: "daemon internal error"})
		}
	}()
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	var req ipcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}

	switch req.Method {
	case cliproto.IPCStatus:
		d.ipcStatus(ctx, conn, req.Params)
	case cliproto.IPCLogin:
		d.ipcLogin(ctx, conn, req.Params)
	case cliproto.IPCCreate:
		d.ipcCreate(ctx, conn, req.Params)
	case cliproto.IPCSwitch:
		d.ipcSwitch(ctx, conn, req.Params)
	case cliproto.IPCSend:
		d.ipcSend(ctx, conn, req.Params)
	case cliproto.IPCSendBatch:
		d.handleSendBatch(ctx, conn, req.Params)
	case cliproto.IPCList:
		d.ipcList(ctx, conn, req.Params)
	case cliproto.IPCListWatch:
		d.handleListWatch(ctx, conn, req.Params)
	case cliproto.IPCSubscribe:
		d.ipcSubscribe(ctx, conn, req.Params)
	case cliproto.IPCRead:
		d.handleRead(ctx, conn, req.Params)
	case cliproto.IPCConnect:
		d.handleConnect(ctx, conn, req.Params)
	case cliproto.IPCDisconnect:
		d.handleDisconnect(ctx, conn, req.Params)
	case cliproto.IPCPipeCreate:
		d.handlePipeCreate(ctx, conn, req.Params)
	case cliproto.IPCPipeDestroy:
		d.handlePipeDestroy(ctx, conn, req.Params)
	case cliproto.IPCSourceDestroy:
		d.handleSourceDestroy(ctx, conn, req.Params)
	case cliproto.IPCSetNamespace:
		d.handleSetNamespace(ctx, conn, req.Params)
	case cliproto.IPCUnsetNamespace:
		d.handleUnsetNamespace(ctx, conn, req.Params)
	case cliproto.IPCDiag:
		d.handleDiag(ctx, conn, req.Params)
	case cliproto.IPCWho:
		d.handleWho(ctx, conn, req.Params)
	case cliproto.IPCScheduleCreate:
		d.handleScheduleCreate(ctx, conn, req.Params)
	case cliproto.IPCScheduleList:
		d.handleScheduleList(ctx, conn, req.Params)
	case cliproto.IPCScheduleRemove:
		d.handleScheduleRemove(ctx, conn, req.Params)
	case cliproto.IPCSubsList:
		d.handleSubsList(ctx, conn, req.Params)
	case cliproto.IPCSubsAdd:
		d.handleSubsAdd(ctx, conn, req.Params)
	case cliproto.IPCSubsRemove:
		d.handleSubsRemove(ctx, conn, req.Params)
	case cliproto.IPCSubsWait:
		d.handleSubsWait(ctx, conn, req.Params)
	case cliproto.IPCComplete:
		d.handleComplete(ctx, conn, req.Params)
	default:
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: "unknown method " + req.Method})
	}
}

func writeIPC(w net.Conn, result any) {
	_ = json.NewEncoder(w).Encode(ipcResponse{Result: result})
}

func writeIPCErr(w net.Conn, e *cliproto.Error) {
	_ = json.NewEncoder(w).Encode(ipcResponse{Error: e})
}

func (d *Daemon) ipcStatus(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.StatusRequest
	_ = json.Unmarshal(params, &req) // optional body — empty is fine
	reply := cliproto.StatusReply{
		DaemonPID:     os.Getpid(),
		DaemonVersion: version.Version,
	}
	reply.NATSState = natsStateString(d.NC)
	// State-since + entry-type from the most recent matching transition
	// in the in-memory event ring. Powers the "(N <unit> ago)" suffix on
	// the `ppz status` `nats:` line and the green/amber/red colouring —
	// see cliproto.formatNATSLine. Nil when the ring has no matching
	// event (fresh daemon, or the transition aged out).
	if d.NATSEvents != nil {
		since, entry := stateSinceFrom(reply.NATSState, d.NATSEvents.Snapshot())
		if !since.IsZero() {
			reply.NATSStateSince = &since
			reply.NATSStateEntry = entry
		}
	}
	if creds, ok := d.State.Credentials(); ok {
		reply.LoggedIn = true
		reply.URL = creds.URL
		reply.KeyPrefix = d.State.KeyPrefix()
		reply.AccountID = d.State.AccountID()
		reply.AccountName = d.State.AccountName()
		if d.Refresh != nil {
			lastRefresh := d.Refresh.LastRefreshAt()
			if !lastRefresh.IsZero() {
				reply.LastTokenRefreshAt = &lastRefresh
			}
		}
		// loginCheck is populated by callServer on every server-touching
		// handler, plus SetLogin (login itself counts as a successful
		// observation). If it's still empty here, no observation has
		// happened yet — probe once so status never lies. A cheap GET
		// /api/v1/sources doubles as the auth check.
		check := d.State.LoginCheck()
		if check == "" {
			var lr cliproto.ListSourcesReply
			if e := d.callServer(ctx, "GET", "/api/v1/sources", nil, &lr); e != nil {
				if e.Code == cliproto.EInvalidAPIKey {
					check = cliproto.LoginCheckInvalid
				}
				// Server-unreachable / other errors leave check empty
				// — the CLI renders that as "unverified" rather than
				// claiming a state we don't actually know.
			} else {
				check = cliproto.LoginCheckOK
			}
		}
		reply.LoginCheck = check
	}
	reply.Current = d.State.Current(req.Session)
	reply.CurrentNamespace = d.State.CurrentNamespace(req.Session)
	reply.CurrentPath = filepath.Join(d.State.Home(), fileCurrent)
	writeIPC(conn, reply)
}

// The remaining ipc* methods are wired in later steps so the package compiles
// today. They reply with a clear "not implemented" error for now.

func (d *Daemon) ipcLogin(ctx context.Context, conn net.Conn, params json.RawMessage) {
	d.handleLogin(ctx, conn, params)
}
func (d *Daemon) ipcCreate(ctx context.Context, conn net.Conn, params json.RawMessage) {
	d.handleCreate(ctx, conn, params)
}
func (d *Daemon) ipcSwitch(ctx context.Context, conn net.Conn, params json.RawMessage) {
	d.handleSwitch(ctx, conn, params)
}
func (d *Daemon) ipcSend(ctx context.Context, conn net.Conn, params json.RawMessage) {
	d.handleSend(ctx, conn, params)
}
func (d *Daemon) ipcList(ctx context.Context, conn net.Conn, params json.RawMessage) {
	d.handleList(ctx, conn, params)
}
func (d *Daemon) ipcSubscribe(ctx context.Context, conn net.Conn, params json.RawMessage) {
	d.handleSubscribe(ctx, conn, params)
}

// IPC client helpers — used by the CLI.

// ipcCallTimeout bounds how long Call waits for the daemon to reply.
// net.Dial over the unix socket succeeds the moment the daemon
// *accepts*, so without a deadline a stalled daemon (e.g. mid-restart,
// before it serves IPC; or wedged on a slow downstream) would block the
// CLI forever — the production "ppz send hung >2min" report. Per the
// send delivery contract clause 2 the CLI must always terminate. The
// default is generous (a legitimate send waits for a JetStream PubAck
// and possible credential refresh) but finite; PPZ_IPC_TIMEOUT overrides
// it for ops, and tests set it small.
var ipcCallTimeout = 30 * time.Second

// Call sends one request to the daemon over a fresh connection and decodes
// either result or error, bounded by ipcCallTimeout (PPZ_IPC_TIMEOUT
// overrides). For request/response verbs that must always terminate.
func Call(sock, method string, params, result any) error {
	timeout := ipcCallTimeout
	if v := os.Getenv("PPZ_IPC_TIMEOUT"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil && d > 0 {
			timeout = d
		}
	}
	return call(context.Background(), sock, method, params, result, timeout)
}

// CallWait is like Call but sets no read deadline (timeout 0), for the
// verbs that legitimately hold the connection open until a NATS event
// arrives (IPCListWatch, IPCSubsWait). The daemon's clientGone goroutine
// cleans up when the connection drops (e.g. process killed via SIGINT),
// so the daemon never leaks. PPZ_IPC_TIMEOUT is intentionally NOT honored
// here — a blocking wait has no meaningful deadline.
//
// Callers that need to abort the wait on a context cancellation (e.g.
// the share-side alert pump goroutine, which has to unblock when the
// wrapped child exits and cmdTerminalShare cancels its ctx) should use
// CallWaitCtx instead. CallWait itself has no ctx hook by design: a
// CLI like `ppz subs wait` blocks forever until SIGINT, and SIGINT
// closes its stdin/stdout naturally so no extra plumbing is required.
func CallWait(sock, method string, params, result any) error {
	return call(context.Background(), sock, method, params, result, 0)
}

// CallWaitCtx is CallWait + context cancellation: when ctx is done
// before the daemon replies, the underlying conn is closed which
// unblocks the in-flight Decode and returns the ctx error wrapped in
// "ipc decode: ...". Required for in-process callers that share a
// context lifecycle with a parent goroutine (the share's alert pump
// loop is the load-bearing case — without ctx hook, the SubsWait
// goroutine blocks the share's wg.Wait() on shutdown).
//
// PPZ_IPC_TIMEOUT is intentionally NOT honored here, same as CallWait.
func CallWaitCtx(ctx context.Context, sock, method string, params, result any) error {
	return call(ctx, sock, method, params, result, 0)
}

// call is the shared IPC client body. When timeout > 0 it bounds the
// read with a connection deadline and classifies a client-side timeout
// as EDaemonTimeout; when timeout == 0 it blocks indefinitely (a
// deadline can't fire, so the Timeout() classification is skipped).
//
// ctx is honored independently of timeout: when ctx is cancelled the
// conn is closed, which surfaces as an "ipc decode" error from
// Decode. Background() ctx (the legacy callers) is a no-op watcher
// that never fires.
func call(ctx context.Context, sock, method string, params, result any, timeout time.Duration) error {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return cliproto.New(cliproto.EDaemonNotRunning)
	}
	defer conn.Close()
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	// Close conn on ctx cancellation so the Decode below returns.
	// Background ctx (legacy callers) never fires this; the goroutine
	// exits via stopCloser when the function returns normally.
	stopCloser := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopCloser:
		}
	}()
	defer close(stopCloser)
	enc := json.NewEncoder(conn)
	enc.SetEscapeHTML(false)
	body, _ := json.Marshal(params)
	if err := enc.Encode(ipcRequest{Method: method, Params: body}); err != nil {
		return err
	}
	dec := json.NewDecoder(conn)
	var resp ipcResponse
	if err := dec.Decode(&resp); err != nil {
		if timeout > 0 {
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				return cliproto.New(cliproto.EDaemonTimeout)
			}
		}
		return fmt.Errorf("ipc decode: %w", err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result == nil {
		return nil
	}
	raw, _ := json.Marshal(resp.Result)
	return json.Unmarshal(raw, result)
}
