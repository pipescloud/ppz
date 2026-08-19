package daemon

import (
	"sort"
	"sync"
	"time"
)

// HeartbeatEntry is one row of the daemon's heartbeat cache —
// effectively "the most recent heartbeat we forwarded for this source".
// `ppz who` formats one row per entry.
type HeartbeatEntry struct {
	Handle    string
	AccountID string    // account the beat arrived under; rows render only while it is the current account
	Payload   string    // verbatim heartbeat JSON, as published by the pty wrapper
	ArrivedAt time.Time // wall-clock time the daemon received the publish (not the payload's ts)
}

// HeartbeatCache stores the latest heartbeat per source handle in
// memory. Lifetime is the daemon process — a daemon restart clears
// every entry, and the next beat from each agent re-populates it (so
// `ppz who` may show "offline" for up to one interval after a daemon
// restart). That's deliberate: persistent cross-restart state would
// need DB/server-side support, which is intentionally deferred.
//
// Every stamp is tagged with the account it arrived under, and
// `ppz who` renders only current-account rows (SnapshotAccount). The
// tag — not clear-ordering around login/logout — is what keeps
// cross-account ghosts out: nats.go's Close doesn't join an in-flight
// subscription callback and no lock spans the login sequence, so a
// stale-account stamp can always land "too late"; tagged, it's inert.
type HeartbeatCache struct {
	mu      sync.RWMutex
	entries map[string]HeartbeatEntry
}

func NewHeartbeatCache() *HeartbeatCache {
	return &HeartbeatCache{entries: map[string]HeartbeatEntry{}}
}

// Stamp upserts the latest heartbeat for handle, tagged with the
// account it was received under. Repeated calls for the same handle
// overwrite; only the freshest beat survives.
func (c *HeartbeatCache) Stamp(handle, accountID, payload string, arrivedAt time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[handle] = HeartbeatEntry{Handle: handle, AccountID: accountID, Payload: payload, ArrivedAt: arrivedAt}
}

// Clear drops every entry. Called on logout (see watchState): with no
// current account there is nothing the rows could render under, and
// keeping them would resurrect stale ghosts on the next same-account
// login. Post-clear behaviour matches a daemon restart: each live
// agent's next beat repopulates the cache within one interval.
func (c *HeartbeatCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]HeartbeatEntry{}
}

// DropOtherAccounts removes every entry not tagged with accountID.
// Called after a login (see the account-boundary comment in
// handleLogin): rows from other accounts must not resurface as stale
// ghosts if the user later switches back, while rows already received
// under the new account — e.g. via a concurrently-armed subscription —
// survive.
func (c *HeartbeatCache) DropOtherAccounts(accountID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for handle, e := range c.entries {
		if e.AccountID != accountID {
			delete(c.entries, handle)
		}
	}
}

// Snapshot returns all entries sorted by handle (ASCII order),
// regardless of account tag. Returns an empty slice (never nil) so
// callers can range freely. Cache-level inspection only — user-facing
// paths (`ppz who`) go through SnapshotAccount.
func (c *HeartbeatCache) Snapshot() []HeartbeatEntry {
	if c == nil {
		return []HeartbeatEntry{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]HeartbeatEntry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out
}

// SnapshotAccount returns the entries tagged with accountID, sorted by
// handle. An empty accountID (logged out) matches nothing — a
// logged-out daemon has no account to render rows under.
func (c *HeartbeatCache) SnapshotAccount(accountID string) []HeartbeatEntry {
	if c == nil || accountID == "" {
		return []HeartbeatEntry{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]HeartbeatEntry, 0, len(c.entries))
	for _, e := range c.entries {
		if e.AccountID == accountID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out
}

// applyHeartbeatStamp is the hook handleSend / handleSendBatch call on
// every publish. Stamps the cache when channel == "heartbeat";
// otherwise no-op. accountID is the account the publish went to (the
// subject's account at resolve time). nil cache is tolerated so the
// daemon can be constructed in any order without race-y panics.
func applyHeartbeatStamp(cache *HeartbeatCache, channel, handle, accountID, payload string, arrivedAt time.Time) {
	if channel != "heartbeat" {
		return
	}
	cache.Stamp(handle, accountID, payload, arrivedAt)
}
