package daemon

import (
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// watchEntry is one live core-NATS subscription anchored to the
// daemon's NC, owned by an in-flight handleSubsWait or handleListWatch
// handler. The cb is the same closure passed to nats.Subscribe at
// handler entry (typically a non-blocking wakeup poke). subject is
// kept verbatim so rearmAll can resubscribe on a new NC without the
// handler needing to be still running.
//
// sub is guarded by watchRegistry.mu — set by add() at handler entry,
// swapped by swapSub() during rearmAll, and read+Unsubscribed by
// remove() at handler exit. The mutex is the only synchronisation the
// entry uses; no other field is mutated after construction.
type watchEntry struct {
	subject string
	cb      nats.MsgHandler
	sub     *nats.Subscription
}

// watchRegistry tracks live core-NATS subs from handleSubsWait /
// handleListWatch so the daemon can re-arm them on a fresh NC when
// swapNCLocked replaces the connection.
//
// Analogous to followRegistry (follow_registry.go) but rearms in
// place instead of evicting. JetStream follows can't be rearmed — the
// consumer lives server-side anchored to the connection, so the only
// fix is to drop the IPC conn and let the CLI's outer redial loop
// reconnect (which followRegistry does via closeAll). Core subs
// (nc.Subscribe) live client-side and can be cheaply resubscribed on
// a different conn, so the IPC conn stays up and the single-shot
// `daemon.Call` semantics of `ppz subs wait` / `ppz ls --watch`
// survive a JWT-rotation NC swap.
//
// Without this registry, oldNC.Close() during swapNCLocked silently
// invalidates wait/watch subscriptions: the handler's wakeup channel
// never fires, and any message arriving on newNC has no subscriber.
// The handler sits until the CLI's 30s IPC deadline (`ipcCallTimeout`
// in ipc.go) — the silent-loss bug observed in the post-rotation-
// auth-violation diagnostics where ~80 NC swaps in 12h compounded.
type watchRegistry struct {
	mu      sync.Mutex
	entries map[*watchEntry]struct{}
}

func newWatchRegistry() *watchRegistry {
	return &watchRegistry{entries: make(map[*watchEntry]struct{})}
}

// add registers e. Caller subscribes on the live NC first (outside
// the registry lock), stores the sub onto e.sub, then calls add().
// See handleSubsWait / handleListWatch for the post-add NC recheck
// that self-heals when a swap landed between Subscribe and add.
func (r *watchRegistry) add(e *watchEntry) {
	r.mu.Lock()
	r.entries[e] = struct{}{}
	r.mu.Unlock()
}

// remove drops e from the registry and Unsubscribes its current sub.
// Idempotent — safe to call defer-style from the handler regardless
// of whether add() succeeded or rearmAll has run in the meantime.
func (r *watchRegistry) remove(e *watchEntry) {
	r.mu.Lock()
	if _, ok := r.entries[e]; ok {
		delete(r.entries, e)
	}
	old := e.sub
	e.sub = nil
	r.mu.Unlock()
	if old != nil {
		_ = old.Unsubscribe()
	}
}

// swapSub atomically installs newSub onto e and returns the previous
// sub for the caller to Unsubscribe. Returns nil if e was removed
// from the registry in the meantime — in that case the caller MUST
// Unsubscribe newSub (it's a leak otherwise). This is the only
// closure of the "handler-exit-during-rearm" race documented in the
// fix plan §Race analysis.
func (r *watchRegistry) swapSub(e *watchEntry, newSub *nats.Subscription) *nats.Subscription {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[e]; !ok {
		return nil
	}
	old := e.sub
	e.sub = newSub
	return old
}

// contains reports whether e is currently registered. Test helper.
func (r *watchRegistry) contains(e *watchEntry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entries[e]
	return ok
}

// rearmAll resubscribes every live entry on newNC, then Unsubscribes
// the previous sub. Called from swapNCLocked AFTER d.NC = newNC has
// been installed but BEFORE oldNC.Close(): for the duration of this
// method each entry has a sub on BOTH NCs, so a message arriving
// mid-swap is observed regardless of which conn the server delivers
// on — no message-arrival gap.
//
// Failure-mode (Subscribe error on newNC): warn is invoked with a
// reason string, the entry is left with its pre-swap sub (about to
// die), and the entry stays in the registry. The next swap retries;
// worst case the handler hits its IPC deadline — same as pre-fix
// behaviour, never worse. warn may be nil (test convenience).
//
// newNC may be nil (logout / swap-to-nil): rearmAll is a no-op in
// that case. The waiting handlers see clientGone or ctx.Done() fire
// when the daemon shuts down.
func (r *watchRegistry) rearmAll(newNC *nats.Conn, warn func(reason string)) {
	if newNC == nil {
		return
	}
	r.mu.Lock()
	entries := make([]*watchEntry, 0, len(r.entries))
	for e := range r.entries {
		entries = append(entries, e)
	}
	r.mu.Unlock()

	for _, e := range entries {
		newSub, err := newNC.Subscribe(e.subject, e.cb)
		if err != nil {
			if warn != nil {
				warn("rearm subscribe failed for " + e.subject + ": " + err.Error())
			}
			continue
		}
		if oldSub := r.swapSub(e, newSub); oldSub != nil {
			_ = oldSub.Unsubscribe()
		} else {
			// Entry removed mid-rearm — newSub would leak. See §Race 4.
			_ = newSub.Unsubscribe()
		}
	}
}

// armWatch creates a core-NATS subscription anchored to the daemon's
// current NC, registers it with d.Watches, and returns the entry —
// callers MUST defer d.Watches.remove(entry) on handler exit.
//
// Holds ncMu across the whole operation (capture → Subscribe → add),
// making it mutually exclusive with swapNCLocked. That collapses the
// race class an earlier "post-add recheck" version of this function
// tried to patch: a swap landing between the NC capture and the
// registry add() left our sub on the about-to-die NC AND outside the
// registry rearmAll could rescue. The recheck itself could lose a
// further race against a second swap and strand the entry on a closed
// conn — see PR review on #115. ncMu is the canonical guard for
// d.NC; rearmAll already calls nc.Subscribe under it, so this is
// consistent with the existing invariant.
//
// nc.Subscribe is local-only (no daemon callbacks), so holding ncMu
// for its duration introduces no reentrancy risk and matches the
// rearmAll pattern.
//
// Returns ENATSUnreachable on no-connection or Subscribe failure —
// the same error class the handlers raised before this refactor.
func (d *Daemon) armWatch(subject string, cb nats.MsgHandler) (*watchEntry, *cliproto.Error) {
	d.ncMu.Lock()
	defer d.ncMu.Unlock()
	if d.NC == nil {
		// Same reasoning as jetStream(): report the recorded dial cause
		// rather than a blanket unreachable.
		return nil, d.noConnErrLocked()
	}
	sub, err := d.NC.Subscribe(subject, cb)
	if err != nil {
		return nil, cliproto.New(cliproto.ENATSUnreachable)
	}
	entry := &watchEntry{subject: subject, cb: cb, sub: sub}
	d.Watches.add(entry)
	return entry, nil
}
