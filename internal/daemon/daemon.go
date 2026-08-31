package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// Daemon is the singleton process per $PPZ_HOME.
type Daemon struct {
	Home    string
	Sock    string
	State   *State
	HTTP    *http.Client
	NC      *nats.Conn // nil until Login
	NATSURL string
	Cursors *cursors

	// Subs holds the per-session pipe-subscription lists backing
	// `ppz subs {ls,add,rm,wait,read}`. File-backed under
	// <PPZ_HOME>/subs/, mirroring Cursors. See subs.go.
	Subs *subscriptions

	// Phase 3.5 — JWT refresh loop. Started on Login (and restored
	// in ensureNATS for daemon restarts), holds the latest minted
	// (jwt, seed) for the current org, and re-runs /auth/exchange
	// at exp-30s. nats.Connect's UserJWT callback reads from
	// Refresh.Current() so reconnects pick up fresh credentials.
	// Stopped on Logout / replaced on Login.
	Refresh *RefreshLoop

	// Phase 0 (agent hardening) — short tail of NATS connection-state
	// transitions. Surfaced by `ppz status` (latest state) and
	// `ppz diagnostics` (full ring). Initialised in New() so the handlers
	// registered on the very first nats.Connect have a non-nil ring
	// to append to.
	NATSEvents *NATSEventRing

	// Heartbeats holds the most recent <handle>.heartbeat payload per
	// source, populated by handleSend / handleSendBatch on the fly.
	// Read by `ppz who`. Memory-only — cleared on daemon restart.
	Heartbeats *HeartbeatCache

	// Follows tracks live IPC conns that handleRead is streaming
	// JetStream events on (Follow: true). Used by swapNC to evict
	// stale follows when the NATS connection is replaced — the old
	// consumers go silent and the CLI needs to redial against the
	// new NC. See follow_registry.go.
	Follows *followRegistry

	// Watches tracks live core-NATS subs from handleSubsWait and
	// handleListWatch. Used by swapNCLocked to re-arm them on the new
	// NC when the connection is replaced — without this, oldNC.Close()
	// silently invalidates the sub, the wakeup chan never fires, and
	// the handler hangs until its 30s IPC deadline (the silent-loss
	// bug surfaced by the post-rotation-auth-violation diagnostics
	// where ~80 NC swaps in 12h compounded). See watch_registry.go.
	Watches *watchRegistry

	// ncMu serializes every NC rebuild/swap so the refresh goroutine
	// (OnRefreshed) and any number of concurrent ensureNATS callers
	// coalesce into a single reconnect per JWT rotation instead of
	// racing — the burst-swap-storm fix (see rebuildNC).
	ncMu sync.Mutex
	// ncExp is the JWT exp d.NC was dialed against. A mismatch with the
	// live Refresh.JWTExp() means creds rotated and NC must be rebuilt;
	// matching means a concurrent caller already did it (coalesce).
	ncExp int64
	// ncGen is the RefreshLoop generation d.NC was dialed against.
	// Checked alongside ncExp because exp is unix SECONDS: two refreshes
	// in the same second produce an identical exp, and coalescing on exp
	// alone would skip the redial and leave the daemon on the previous
	// credential. Under ACL enforcement that means holding access that
	// has already been narrowed.
	ncGen uint64
	// lastDialErr is why the most recent dial failed, or nil after a
	// successful one. Guarded by ncMu and read in the same critical
	// section as NC, so "no connection" can always be reported with its
	// cause instead of a generic unreachable.
	lastDialErr *cliproto.Error

	// aclEnforced mirrors the org's enforcement state from the last
	// /auth/exchange. Read by the list path to tell a refusal apart
	// from a transport fault — see isEnumerationDenied.
	aclEnforced atomic.Bool
	// dial builds a fresh NATS connection; injectable so tests can
	// substitute a stub. Defaults to connectNATSWithRefresh.
	dial func(url string, r *RefreshLoop, store func(NATSEvent)) (*nats.Conn, error)

	// wakeRetryInterval overrides the backoff between credential
	// refresh attempts in onWake (see wake_watchdog.go). Zero means
	// the production default (wakeRefreshRetryInterval); tests set
	// milliseconds.
	wakeRetryInterval time.Duration

	// reconnecting is the single-flight guard for the background
	// reconnect loop (kickReconnect): a failure-close may be reported
	// by many concurrent operations, but only one recovery loop runs.
	reconnecting atomic.Bool

	// reportingFailure single-flights reportNATSFailure's verification
	// probe: N concurrent commands tripping the same JetStream blip
	// coalesce onto ONE probe and ONE ring event. Later callers return
	// immediately and let the in-flight report decide the connection's
	// fate — without this they would serially queue behind each other's
	// probes (N × jsAPITimeout worst case) and each stamp their own
	// warn/force_close for a single incident.
	reportingFailure atomic.Bool

	// reconnectBackoff overrides the initial backoff of the background
	// connect/recovery loop (kickConnect). Zero means the production
	// default (reconnectInitialBackoff); tests set milliseconds.
	reconnectBackoff time.Duration

	// bootstrapMu single-flights the cold-start section of ensureNATS —
	// refresh-loop init + the one-time /auth/exchange that populates
	// NATSURL and the refresh loop. Without it, connectOnStartup's
	// background ensureNATS and the first IPC command's ensureNATS can
	// both observe NATSURL=="" and run the cold block concurrently,
	// double-calling /auth/exchange and racing the plain-field writes to
	// d.NATSURL / d.Refresh (startRefreshLoop twice → orphaned refresh
	// goroutine). Distinct from ncMu, which guards only the connection
	// swap: ensureNATS must NOT hold ncMu across the HTTP exchange.
	// Lock order is always bootstrapMu → ncMu (via OnRefreshed →
	// rebuildNC), never the reverse.
	//
	// NOTE: bootstrapMu serializes the cold WRITERS of d.NATSURL; it does
	// NOT make the field itself race-free (it's read under ncMu in
	// rebuildNC and unlocked in handleLogin/diagSummary). This is benign
	// only because d.NATSURL is write-once-then-constant: it goes
	// ""→<url> exactly once per daemon (the cold block re-enters only
	// while empty), then never changes, so the single transition is the
	// only race window and a torn/empty read just retries the dial. If
	// NATSURL ever becomes mutable mid-life (per-org endpoints, region
	// failover, server re-pointing a live daemon), this stops being
	// write-once and must move to one consistent lock or
	// atomic.Pointer[string] across all NATSURL sites.
	bootstrapMu sync.Mutex

	// baseCtx is the daemon-lifetime context, set once at the top of
	// Run(). Background recovery loops kicked from places without a
	// request context (reportNATSFailure, the ClosedHandler) use it so
	// they stop when the daemon shuts down. Nil outside Run() (unit
	// tests) — callers fall back to context.Background().
	baseCtx context.Context

	// completionWarmMu serialises cold-cache warming inside
	// handleComplete. Without it, N concurrent tab presses on a fresh
	// daemon all read CompletionSnapshot()=false and race to fire
	// GET /api/v1/sources. The lock is only contended on the very
	// first call(s); steady-state tabs see a warm cache and skip the
	// lock entirely. A sync.Mutex (over sync.Once) is deliberate: if
	// the first probe fails (server unreachable), the next caller
	// retries, where Once.Do would lock the cache cold forever.
	completionWarmMu sync.Mutex
}

func New(home, sock string) *Daemon {
	d := &Daemon{
		Home:       home,
		Sock:       sock,
		State:      NewState(home),
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		Cursors:    newCursors(home),
		Subs:       newSubscriptions(home),
		NATSEvents: newNATSEventRing(natsEventRingCap),
		Heartbeats: NewHeartbeatCache(),
		Follows:    newFollowRegistry(),
		Watches:    newWatchRegistry(),
		dial:       connectNATSWithRefresh,
	}
	// Subs are account-scoped (subs/<account>/<session>.json); wired
	// here because the literal above can't reference d.State.
	d.Subs.account = d.State.AccountID
	return d
}

// dialer returns d.dial with the production fallback. New() always
// injects connectNATSWithRefresh, but Daemon literals (tests) may leave
// dial nil — every dial site goes through here so none can nil-deref.
func (d *Daemon) dialer() func(string, *RefreshLoop, func(NATSEvent)) (*nats.Conn, error) {
	if d.dial != nil {
		return d.dial
	}
	return connectNATSWithRefresh
}

// rebuildNC ensures d.NC is connected with the current JWT generation:
// if the connection is missing, disconnected, or was dialed against an
// older JWT exp than the live Refresh, it dials a fresh connection and
// swaps it in. Both the on-demand path (ensureNATS) and the proactive
// refresh path (OnRefreshed) route through here so a JWT rotation triggers
// exactly ONE reconnect — not N racing ones (the burst-swap-storm).
//
// Serialized via ncMu with a double-check: the first caller to find the NC
// stale dials + swaps; everyone else blocks, then re-checks and finds the
// connection current and no-ops. That collapses the rotation thundering
// herd into a single reconnect.
func (d *Daemon) rebuildNC(caller string) error {
	d.ncMu.Lock()
	defer d.ncMu.Unlock()
	if d.NATSURL == "" {
		return nil // nothing to dial yet (pre-login / pre-bootstrap)
	}
	if d.NC != nil && d.NC.IsConnected() &&
		d.ncExp == d.Refresh.JWTExp() && d.ncGen == d.Refresh.Generation() {
		return nil // already connected on the current generation — coalesce
	}
	nc, err := d.dialer()(d.NATSURL, d.Refresh, d.recordNATSEvent)
	if err != nil || nc == nil {
		// Classify rather than flatten: an oversized credential is
		// deterministic and server-side, and telling the user to
		// re-login (E_NATS_UNREACHABLE's advice) sends them round a loop
		// that cannot terminate. Recorded so the call sites that later
		// find no connection installed can say why.
		cerr := natsDialError(err)
		d.setDialErrLocked(cerr)
		return cerr
	}
	d.clearDialErrLocked()
	d.swapNCLocked(caller, nc) // stamps d.ncExp and emits any transition event
	if aid, perr := uuid.Parse(d.State.AccountID()); perr == nil {
		_, _ = d.subscribePresence(aid)
		_, _ = d.subscribeSystem(aid)
	}
	return nil
}

// swapNC replaces d.NC, first evicting every live follow conn so the
// CLI redials against the fresh NATS connection. Centralizing this
// pattern guarantees that watcher / handleLogin / ensureNATS all
// invalidate stale follows identically; any future NC-replacement
// path picks up the same behaviour for free.
//
// `newNC` may be nil — used by the watcher when credentials disappear
// and there's no replacement to install. Callers are responsible for
// any nats.Conn lifecycle (connectNATSWithRefresh ownership) outside
// of swapping the pointer.
//
// caller is the originating function name ("ensureNATS",
// "OnRefreshed-callback", "handleLogin", ...), recorded on the
// emitted "swap" event so readers can attribute every NC transition.
// The swap event captures both the old and new NC IDs in Reason so a
// single line tells the full story; the inevitable nats.go-driven
// disconnect+closed pair (from closing the old NC) lands separately
// with Caller="nats.go" — that's the noise the burst-swap-storm
// pattern detects.
// swapNC is the lock-acquiring entry point for standalone NC replacements
// (handleLogin, watcher) that don't already hold ncMu. Callers inside a
// ncMu-held critical section (rebuildNC) use swapNCLocked directly.
func (d *Daemon) swapNC(caller string, newNC *nats.Conn) {
	d.ncMu.Lock()
	defer d.ncMu.Unlock()
	d.swapNCLocked(caller, newNC)
}

// swapNCLocked performs the actual close-old / install-new / evict-follows
// swap. The caller MUST hold d.ncMu.
//
// Emits up to two events: a "swap" capturing the pointer transition for
// burst-swap-storm detection, plus — when the swap actually represents a
// connectivity transition — a "connect" or "reconnect" anchoring the
// `ppz status` / `ppz diagnostics` state-since reading. The transition
// classification:
//
//   - old==nil, new!=nil → "connect"   (initial / post-logout login)
//   - old disconnected, new!=nil → "reconnect" (recovery)
//   - old healthy, new!=nil → no anchor event (routine JWT-refresh swap)
//   - new==nil → no anchor event (logout / teardown)
//
// nats.go never fires a ConnectHandler in our setup, so the daemon is
// the sole emitter of "connect" — centralising it here means every
// caller (handleLogin, rebuildNC, watcher) gets it for free without
// having to remember the classification.
func (d *Daemon) swapNCLocked(caller string, newNC *nats.Conn) {
	oldNC := d.NC
	oldID := ncID(oldNC)
	newID := ncID(newNC)
	// Capture connectivity BEFORE Close() — IsConnected() flips to false
	// once we close the old conn, so we'd misclassify a routine JWT-
	// refresh swap (healthy → healthy) as a "reconnect" without this.
	oldWasConnected := oldNC != nil && oldNC.IsConnected()
	if d.NATSEvents != nil {
		d.recordNATSEvent(NATSEvent{
			Type:   "swap",
			At:     time.Now(),
			Caller: caller,
			NCID:   newID,
			JWTExp: d.Refresh.JWTExp(),
			// swapReason, not an inline string: drops_last_hour's
			// swap-attribution parses this back (see swapReasonRetired).
			Reason: swapReason(oldID, newID),
		})
	}
	if d.Follows != nil {
		d.Follows.closeAll()
	}
	// Install newNC BEFORE rearming watches and closing oldNC: any
	// handler racing with this swap that captures d.NC under ncMu
	// after we release it must see the new conn, not a transient nil.
	// (We hold ncMu here, so no concurrent reader can interleave; the
	// ordering matters only for the rearmAll loop below, which itself
	// re-Subscribes on newNC.)
	d.NC = newNC
	// Rearm wait/watch core-NATS subs on the new conn before closing
	// the old one. For the duration of rearmAll each entry has a sub
	// on BOTH conns, so a message arriving mid-swap is observed
	// regardless of which conn the server delivers on — no message-
	// arrival gap. Failure-mode is fail-soft (see watch_registry.go);
	// nil-safe so the swap path stays functional when tests construct
	// a Daemon literal without Watches.
	if d.Watches != nil {
		d.Watches.rearmAll(newNC, func(reason string) {
			if d.NATSEvents != nil {
				d.recordNATSEvent(NATSEvent{
					Type:   "warn",
					At:     time.Now(),
					Caller: "rearmWatches/" + caller,
					NCID:   newID,
					JWTExp: d.Refresh.JWTExp(),
					Reason: reason,
				})
			}
		})
		// Flush newNC so the server has acked our SUBs BEFORE we close
		// oldNC. Without this, the "live sub on both conns" guarantee
		// only holds client-side: the server can process oldNC's close
		// before newNC's still-buffered SUBs, dropping any publish that
		// lands in the sliver — the same silent-loss failure mode, just
		// rarer. The deadline is a defence against a wedged newNC; on
		// timeout we proceed (the close fires anyway, fail-soft).
		if newNC != nil {
			_ = newNC.FlushTimeout(2 * time.Second)
		}
	}
	if oldNC != nil && oldNC != newNC {
		oldNC.Close()
	}
	// Stamp the JWT generation this connection was dialed against so the
	// rebuildNC double-check (ncExp vs Refresh.JWTExp) is accurate for EVERY
	// swap path — handleLogin and the watcher included, not just rebuildNC.
	// Without this, the first ensureNATS after login sees ncExp=0 != JWTExp
	// and does a redundant reconnect on a healthy connection. A nil swap
	// (creds gone) clears the generation.
	if newNC != nil {
		d.ncExp = d.Refresh.JWTExp()
		d.ncGen = d.Refresh.Generation()
	} else {
		d.ncExp = 0
		d.ncGen = 0
	}
	if d.NATSEvents != nil && newNC != nil {
		switch {
		case oldNC == nil:
			d.recordNATSEvent(NATSEvent{
				Type:   "connect",
				At:     time.Now(),
				Caller: caller,
				NCID:   newID,
				JWTExp: d.Refresh.JWTExp(),
				Reason: "initial connection",
			})
		case !oldWasConnected:
			d.recordNATSEvent(NATSEvent{
				Type:   "reconnect",
				At:     time.Now(),
				Caller: caller,
				NCID:   newID,
				JWTExp: d.Refresh.JWTExp(),
				Reason: "rebuilt connection",
			})
		}
	}
}

// reportNATSFailure is called when a JetStream operation times out on a
// nominally-connected NC. That has two very different causes, and the
// 2026-08-19 incident is why we now tell them apart:
//
//   - The "zombie connection" (#116): TCP/core alive, nc.Status() ==
//     CONNECTED, but the server's JetStream tier is not responding
//     (e.g. JWT just expired server-side). Dead for real — close it so
//     natsStateString stops lying "connected" and recovery rebuilds.
//   - One slow $JS.API reply on a perfectly healthy connection. The
//     pre-incident code closed here too, converting a 5s latency blip
//     into a genuine connection drop for every concurrent command —
//     and the only ring trace was an unattributed caller="nats.go"
//     disconnect with reason="" (nats.Conn.Close dispatches its
//     callbacks with a nil error).
//
// So: VERIFY before killing. A fresh JetStream round-trip on the same
// conn separates the two — a true zombie fails it, a healthy conn with
// one slow reply passes. Probe passes → keep the connection, record a
// "warn" so diagnostics can still explain the caller's error. Probe
// fails → record an attributed "force_close" carrying cause (the
// JetStream error that triggered the report), THEN close and kick the
// background reconnect — a closed NC never recovers by itself (nats.go's
// CLOSED state is terminal), and waiting for "the next ensureNATS" means
// waiting for the user to type a command (the 2026-06-11 incident).
//
// Worst-case added latency is one jsAPITimeout for the probe, on a path
// that already burned a JetStream timeout — acceptable for keeping the
// daemon's one connection alive. No lock is held across the probe.
func (d *Daemon) reportNATSFailure(cause error) {
	d.ncMu.Lock()
	nc := d.NC
	d.ncMu.Unlock()
	if nc == nil || !nc.IsConnected() {
		return
	}
	// Single-flight: a stampede of commands failing on the same blip
	// coalesces onto the report already in flight (see reportingFailure).
	if !d.reportingFailure.CompareAndSwap(false, true) {
		return
	}
	defer d.reportingFailure.Store(false)
	reason := "unknown"
	if cause != nil {
		reason = cause.Error()
	}
	probeErr := probeJetStream(nc)
	if probeErr == nil {
		d.recordNATSEvent(NATSEvent{
			Type:   "warn",
			At:     time.Now(),
			Caller: "reportNATSFailure",
			NCID:   ncID(nc),
			JWTExp: d.Refresh.JWTExp(),
			Reason: "kept connection: probe passed after " + reason,
		})
		return
	}
	d.recordNATSEvent(NATSEvent{
		Type:   "force_close",
		At:     time.Now(),
		Caller: "reportNATSFailure",
		NCID:   ncID(nc),
		JWTExp: d.Refresh.JWTExp(),
		Reason: reason + "; probe: " + probeErr.Error(),
	})
	// The probe ran lock-free; a concurrent swap may have retired nc
	// already. If so, the daemon is healthy on a newer conn — closing
	// the retired one again would only fire a second orphan close.
	d.ncMu.Lock()
	stillCurrent := d.NC == nc
	d.ncMu.Unlock()
	if !stillCurrent {
		return
	}
	nc.Close()
	d.kickReconnect()
}

// probeJetStream performs one fresh JetStream API round-trip on nc,
// bounded by jsAPITimeout. nil means the JetStream tier answered — the
// connection is usable and a preceding op timeout was a blip, not a
// zombie. AccountInfo is the cheapest $JS.API request there is, and its
// failure modes cover both zombie shapes: a wedged transport times out,
// a dead/disabled JetStream tier errors immediately (no responders).
// A var so tests can hold a probe mid-flight (single-flight coverage).
var probeJetStream = func(nc *nats.Conn) error {
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), jsAPITimeout)
	defer cancel()
	_, err = js.AccountInfo(ctx)
	return err
}

// reconnectInitialBackoff / reconnectMaxBackoff bound the background
// reconnect loop's retry cadence: fast enough that recovery lands
// within seconds of the server becoming reachable, capped so an
// extended outage doesn't hammer it.
const (
	reconnectInitialBackoff = 2 * time.Second
	reconnectMaxBackoff     = 15 * time.Second
)

// connectOnStartup brings the NATS connection up at daemon startup for
// an already-logged-in daemon. Without it, Run() leaves the connection
// to be established lazily by the first IPC command that calls
// ensureNATS — so a restarted daemon (upgrade, reboot, crash) sits at
// `nats: unknown`, subscribed to nothing and receiving no pushed
// messages, until the operator happens to run a command. Delegates to
// the single-flight connect loop, which bootstraps a cold daemon (no
// refresh loop / no NATS URL yet) via ensureNATS.
func (d *Daemon) connectOnStartup(ctx context.Context) {
	d.kickConnect(ctx, "startup")
}

// kickReconnect starts the background NATS recovery loop after a
// connection is lost (a JetStream-op failure-close via
// reportNATSFailure, or a terminal close observed by recordNATSEvent).
// It uses the daemon-lifetime context so recovery ends at shutdown.
func (d *Daemon) kickReconnect() {
	ctx := d.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	d.kickConnect(ctx, "reconnect")
}

// kickConnect runs the single-flight background connect/recovery loop:
// concurrent callers (startup, reportNATSFailure, the ClosedHandler)
// coalesce onto ONE loop. Each iteration calls ensureNATS — the
// canonical "make sure we're connected, bootstrapping creds + NATS URL
// if cold" operation — which coalesces with command-driven ensureNATS
// callers and the refresh goroutine via ncMu. The loop retries
// transient failures (server / NATS unreachable) with capped backoff
// and exits on: connected, logged out, or bearer revoked.
func (d *Daemon) kickConnect(ctx context.Context, caller string) {
	if _, ok := d.State.Credentials(); !ok {
		return // not logged in — nothing to connect (pre-login / post-logout)
	}
	if !d.reconnecting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer d.reconnecting.Store(false)
		backoff := reconnectInitialBackoff
		if d.reconnectBackoff > 0 {
			backoff = d.reconnectBackoff
		}
		for {
			if _, ok := d.State.Credentials(); !ok {
				return // logged out mid-recovery
			}
			err := d.ensureNATS(ctx)
			if err == nil {
				d.ncMu.Lock()
				connected := d.NC != nil && d.NC.IsConnected()
				d.ncMu.Unlock()
				if connected {
					return
				}
			} else {
				// Revoked bearer / not-logged-in is terminal — retrying
				// can't fix it. Everything else (server / NATS
				// unreachable) is transient: back off and retry.
				var ce *cliproto.Error
				if errors.As(err, &ce) && (ce.Code == cliproto.EInvalidAPIKey || ce.Code == cliproto.ENotLoggedIn) {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > reconnectMaxBackoff {
				backoff = reconnectMaxBackoff
			}
		}
	}()
}

// recordNATSEvent is the single sink for connection-state events: it
// stamps the in-memory ring (hot tail for `ppz status` /
// `ppz diagnostics`) AND appends to the on-disk jsonl (full history,
// surviving restarts and burst-rate aging). Failures on the disk path
// are silent — observability is best-effort.
func (d *Daemon) recordNATSEvent(ev NATSEvent) {
	if d.NATSEvents != nil {
		d.NATSEvents.Append(ev)
	}
	if d.Home != "" {
		_ = appendNATSEventLog(d.Home, ev)
	}
	// A terminal close (server kick / "Authorization Violation" / a drop
	// nats.go gave up on) is otherwise just a log line: a background
	// close with no active command would wait for the next JWT rotation
	// to rebuild — up to ~4.5 min awake, far longer across sleep (the
	// 2026-06-15 diagnostics). Kick the single-flight reconnect. It
	// no-ops when the daemon is already healthy on a newer connection —
	// the routine rotation closes the OLD conn, and ensureNATS's
	// generation check coalesces that away — so the common swap-close
	// costs one quick no-op iteration, not churn. kickReconnect is
	// non-blocking (CAS + goroutine), so this is safe even on the
	// swap path where recordNATSEvent runs under ncMu.
	if ev.Type == "closed" {
		d.kickReconnect()
	}
}

// Run runs the daemon in the foreground. Returns when ctx is cancelled or
// the listener fails.
func (d *Daemon) Run(ctx context.Context) error {
	// Daemon-lifetime context for background loops kicked from contexts
	// without a request (reportNATSFailure, the ClosedHandler). Set
	// before anything that could trigger a reconnect.
	d.baseCtx = ctx
	if err := os.MkdirAll(d.Home, 0o700); err != nil {
		return fmt.Errorf("mkdir home: %w", err)
	}
	// PID file (also used by reset.sh for SIGHUP).
	if err := os.WriteFile(filepath.Join(d.Home, filePID), []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}
	defer os.Remove(filepath.Join(d.Home, filePID))

	// Diagnostics ring: prime from BOTH the on-disk lifecycle log (so
	// the previous daemon's stop event is observable here) AND the
	// connection-events tail (so a fresh process's `ppz diagnostics`
	// shows the burst that prompted the operator to restart it).
	// Then record our own start; the stop counterpart fires on
	// shutdown below.
	d.reseedRingFromDisk()
	d.recordDaemonLifecycle("daemon_start", "")
	defer d.recordDaemonLifecycle("daemon_stop", "graceful")

	if err := d.State.LoadFromDisk(); err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// State reload: file-mtime poller + SIGHUP. The poller is what makes
	// out-of-band resets reliable — the test harness runs in a different
	// container (different PID namespace) so kill -HUP can't reach this
	// process. SIGHUP is still honoured for local dev.
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	go d.watchState(ctx, hupCh)

	// Remove any stale socket file from a prior crash.
	_ = os.Remove(d.Sock)
	ln, err := net.Listen("unix", d.Sock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", d.Sock, err)
	}
	defer ln.Close()
	defer os.Remove(d.Sock)
	if err := os.Chmod(d.Sock, 0o600); err != nil {
		return fmt.Errorf("chmod sock: %w", err)
	}

	// Sleep/wake detection: macOS pauses the monotonic clock during
	// sleep, so every pending timer (refresh fire included) resumes
	// LATE after wake. The watchdog spots the wall-clock jump and
	// forces refresh + reconnect immediately. See wake_watchdog.go.
	d.startWakeWatchdog(ctx)

	// Bring NATS up proactively if we're already logged in — don't wait
	// for the first IPC command. A restarted daemon (upgrade, reboot)
	// otherwise sits at `nats: unknown`, subscribed to nothing, until
	// something calls ensureNATS.
	d.connectOnStartup(ctx)

	go d.serveIPC(ctx, ln)

	<-ctx.Done()
	return nil
}

// PIDFromHome reads $PPZ_HOME/daemon.pid. Returns 0 if absent.
func PIDFromHome(home string) int {
	data, err := os.ReadFile(filepath.Join(home, filePID))
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscanf(string(data), "%d", &pid)
	return pid
}

// IsAlive returns true if the PID-bearing process exists.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

var ErrCredsRequired = errors.New("credentials required")
