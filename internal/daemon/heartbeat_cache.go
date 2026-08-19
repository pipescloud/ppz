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
	Payload   string    // verbatim heartbeat JSON, as published by the pty wrapper
	ArrivedAt time.Time // wall-clock time the daemon received the publish (not the payload's ts)
}

// HeartbeatCache stores the latest heartbeat per source handle in
// memory. Lifetime is the daemon process — a daemon restart clears
// every entry, and the next beat from each agent re-populates it (so
// `ppz who` may show "offline" for up to one interval after a daemon
// restart). That's deliberate: persistent cross-restart state would
// need DB/server-side support, which is intentionally deferred.
type HeartbeatCache struct {
	mu      sync.RWMutex
	entries map[string]HeartbeatEntry
}

func NewHeartbeatCache() *HeartbeatCache {
	return &HeartbeatCache{entries: map[string]HeartbeatEntry{}}
}

// Stamp upserts the latest heartbeat for handle. Repeated calls for the
// same handle overwrite; only the freshest beat survives.
func (c *HeartbeatCache) Stamp(handle, payload string, arrivedAt time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[handle] = HeartbeatEntry{Handle: handle, Payload: payload, ArrivedAt: arrivedAt}
}

// Clear drops every entry. Called when the daemon crosses an account
// boundary — cross-account login and logout; see the account-boundary
// comment in handleLogin for the full rationale and the ordering it
// must run in. Post-clear behaviour matches a daemon restart: each
// live agent's next beat repopulates the cache within one interval.
func (c *HeartbeatCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]HeartbeatEntry{}
}

// Snapshot returns all entries sorted by handle (ASCII order). Returns
// an empty slice (never nil) so callers can range freely.
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

// applyHeartbeatStamp is the hook handleSend / handleSendBatch call on
// every publish. Stamps the cache when channel == "heartbeat";
// otherwise no-op. nil cache is tolerated so the daemon can be
// constructed in any order without race-y panics.
func applyHeartbeatStamp(cache *HeartbeatCache, channel, handle, payload string, arrivedAt time.Time) {
	if channel != "heartbeat" {
		return
	}
	cache.Stamp(handle, payload, arrivedAt)
}
