package daemon

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// natsStateString collapses the rich nats.Conn.Status() enum into the
// three-value vocabulary surfaced by `ppz status`'s `nats:` line.
// CONNECTING / RECONNECTING both render as "connecting" — they're the
// same thing from an operator's perspective ("client is trying").
// CLOSED collapses to "disconnected" — the connection is gone, with
// or without intent to reconnect.
//
// Returns "" when nc is nil (daemon has never attempted to connect,
// e.g. fresh process pre-login).
func natsStateString(nc *nats.Conn) string {
	if nc == nil {
		return ""
	}
	switch nc.Status() {
	case nats.CONNECTED:
		return "connected"
	case nats.CONNECTING, nats.RECONNECTING:
		return "connecting"
	case nats.DISCONNECTED, nats.CLOSED:
		return "disconnected"
	default:
		return "disconnected"
	}
}

// natsEventRingCap is the maximum number of NATS connection-state events
// retained in memory. Sized so a few rotation bursts (each ~8-12 events
// under contention — see docs/diagnostics.md "burst-swap-storm") fit
// without aging out the surrounding context. Full history lives on disk
// in `nats-events.jsonl`; the ring is the hot tail used by the default
// `ppz diagnostics` output.
const natsEventRingCap = 256

// NATSEventSchemaVersion is the on-disk + on-wire schema version stamped
// on every NATSEvent. Bumped when fields are renamed or semantics
// change; new fields with json:",omitempty" do not require a bump.
// Reader contract is documented in docs/diagnostics.md.
const NATSEventSchemaVersion = 1

// NATSEvent is one entry in the daemon's NATS connection-state log.
//
// Type vocabulary (closed set — extend with care, document in
// docs/diagnostics.md):
//   - "connect"     — nats.go ConnectHandler fired (initial / reconnect success)
//   - "disconnect"  — nats.go DisconnectErrHandler fired
//   - "reconnect"   — nats.go ReconnectHandler fired
//   - "closed"      — nats.go ClosedHandler fired
//   - "swap"        — daemon code called swapNC (Caller names which fn)
//   - "force_close" — reportNATSFailure verified the conn dead (probe
//     failed) and is about to close it; Reason carries the triggering
//     JetStream error + the probe error, so the nats.go
//     disconnect/closed pair that follows is never an orphan
//   - "warn"        — non-fatal failure (e.g. resubscribe error, or a
//     reportNATSFailure whose probe passed — connection kept)
//   - "daemon_start" / "daemon_stop" — lifecycle hooks
//
// Caller distinguishes daemon-initiated transitions from nats.go-initiated
// ones: "nats.go" for library callbacks, the Go function name otherwise
// ("ensureNATS", "OnRefreshed-callback", "handleLogin", "watchState").
// This is the single most useful field for "who closed this connection?"
// — see the burst-swap-storm pattern.
//
// NCID is the connection's pointer address ("0x14000123abc"), letting a
// reader trace which logical NC each event references across a rotation.
//
// JWTExp is the unix-seconds `exp` of the JWT in use at event time. 0
// means unknown (lifecycle / warn events, or pre-login). Used by the
// post-rotation-auth-violation pattern.
type NATSEvent struct {
	V      int       `json:"v"`
	Type   string    `json:"type"`
	At     time.Time `json:"at"`
	Caller string    `json:"caller,omitempty"`
	NCID   string    `json:"nc_id,omitempty"`
	JWTExp int64     `json:"jwt_exp,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

// ncID renders a *nats.Conn pointer as a stable identity string for
// event correlation. Nil renders as "" so callers don't have to
// nil-check before stamping. Format is implementation-defined; readers
// should treat it as opaque.
func ncID(nc *nats.Conn) string {
	if nc == nil {
		return ""
	}
	return fmt.Sprintf("%p", nc)
}

// NATSEventRing is a fixed-capacity, append-only, drop-oldest ring of
// NATSEvent records. Used by the daemon to surface a recent tail of
// connection-state transitions through `ppz diagnostics`.
//
// Thread-safe: every accessor takes mu. The ring lives on Daemon and
// is initialised in New() — before any nats.Connect call — so the
// handlers we register can append without nil-checking.
type NATSEventRing struct {
	mu     sync.Mutex
	events []NATSEvent
	cap    int
}

func newNATSEventRing(cap int) *NATSEventRing {
	if cap <= 0 {
		cap = natsEventRingCap
	}
	return &NATSEventRing{cap: cap, events: make([]NATSEvent, 0, cap)}
}

// Append records one event. When the ring is full the oldest entry is
// dropped — diag's value is precisely catching transient events that
// happened "a few minutes ago", so we keep the tail and lose the head.
// The full history (beyond ring cap) lives on disk in nats-events.jsonl;
// see nats_events_persistence.go.
//
// V is stamped to NATSEventSchemaVersion if zero, so callers don't have
// to remember to set it on every Append.
func (r *NATSEventRing) Append(ev NATSEvent) {
	if ev.V == 0 {
		ev.V = NATSEventSchemaVersion
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) < r.cap {
		r.events = append(r.events, ev)
		return
	}
	copy(r.events, r.events[1:])
	r.events[len(r.events)-1] = ev
}

// Snapshot returns a copy of the current events in chronological order
// (oldest first). Safe to retain — the returned slice is independent of
// the ring's internal storage.
func (r *NATSEventRing) Snapshot() []NATSEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]NATSEvent, len(r.events))
	copy(out, r.events)
	return out
}

// natsStatusSnapshot derives the fields the `ppz status` "nats:" line
// needs from the daemon's current NATS connection + ring buffer. Lives
// here next to the ring so the dependencies stay co-located.
//
// Returns ("", 0, nil) when the daemon has never attempted a NATS
// connection (NC nil AND no events recorded). The CLI renders that as
// "unknown" — preferable to lying with "disconnected" before any
// connect has been tried.
func (d *Daemon) natsStatusSnapshot() (state string, dropsLastHour int, lastEventAt *time.Time) {
	state = natsStateString(d.NC)
	if d.NATSEvents == nil {
		return state, 0, nil
	}
	events := d.NATSEvents.Snapshot()
	if len(events) > 0 {
		t := events[len(events)-1].At
		lastEventAt = &t
	}
	cutoff := time.Now().Add(-time.Hour)
	for i, ev := range events {
		if ev.Type == "disconnect" && !ev.At.Before(cutoff) && !swapAttributedDisconnect(events, i) {
			dropsLastHour++
		}
	}
	return state, dropsLastHour, lastEventAt
}

// swapDropAttributionWindow bounds how far back a disconnect looks for
// the swap that retired its connection. swapNCLocked records the "swap"
// (then closes the old conn) strictly before nats.go's async handlers
// record the resulting "disconnect", so in practice the pair lands
// within milliseconds; a couple of seconds absorbs handler-dispatch lag
// (e.g. the rearm flush) without reaching back to unrelated swaps.
const swapDropAttributionWindow = 5 * time.Second

// swapAttributedDisconnect reports whether events[i] (a "disconnect") is
// the teardown of a connection the daemon retired ON PURPOSE: a "swap"
// event just before it names the disconnect's NCID as the replaced conn
// ("old=<id> new=..."). Those are routine — every JWT-refresh rotation
// produces one — and counting them made drops_last_hour read ~14 on a
// completely healthy hour (2026-08-19 incident report, question 3).
// Genuine network drops and reportNATSFailure force-closes carry no such
// swap and still count.
func swapAttributedDisconnect(events []NATSEvent, i int) bool {
	ev := events[i]
	if ev.NCID == "" {
		return false
	}
	// The retired id is always followed by " new=" in swap reasons, so
	// matching with the trailing space can't confuse 0xA with 0xAB.
	needle := "old=" + ev.NCID + " "
	for j := i - 1; j >= 0; j-- {
		if ev.At.Sub(events[j].At) > swapDropAttributionWindow {
			return false
		}
		if events[j].Type == "swap" && strings.Contains(events[j].Reason, needle) {
			return true
		}
	}
	return false
}
