package daemon

import (
	"reflect"
	"testing"
	"time"
)

// HeartbeatCache is the daemon's in-memory store of the most recent
// heartbeat per source handle. Populated by handleSend / handleSendBatch
// when channel == "heartbeat"; read by IPCWho.

func TestHeartbeatCache_StampAndSnapshot(t *testing.T) {
	c := NewHeartbeatCache()
	t1 := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 16, 12, 1, 0, 0, time.UTC)
	c.Stamp("alice", "acct-1", `{"seq":1}`, t1)
	c.Stamp("bob", "acct-1", `{"seq":1}`, t2)
	got := c.Snapshot()
	want := []HeartbeatEntry{
		{Handle: "alice", AccountID: "acct-1", Payload: `{"seq":1}`, ArrivedAt: t1},
		{Handle: "bob", AccountID: "acct-1", Payload: `{"seq":1}`, ArrivedAt: t2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot mismatch:\n got=%#v\nwant=%#v", got, want)
	}
}

// Repeated Stamps for the same handle overwrite the previous entry —
// only the latest heartbeat survives in cache.
func TestHeartbeatCache_StampOverwritesPrevious(t *testing.T) {
	c := NewHeartbeatCache()
	t1 := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(60 * time.Second)
	c.Stamp("alice", "acct-1", `{"seq":1}`, t1)
	c.Stamp("alice", "acct-1", `{"seq":2}`, t2)
	got := c.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot len = %d, want 1", len(got))
	}
	if got[0].Payload != `{"seq":2}` {
		t.Errorf("Payload = %q, want seq:2", got[0].Payload)
	}
	if !got[0].ArrivedAt.Equal(t2) {
		t.Errorf("ArrivedAt = %v, want %v", got[0].ArrivedAt, t2)
	}
}

// Snapshot is sorted by handle — ascii order, deterministic — so callers
// (notably `ppz who` rendering) don't have to re-sort.
func TestHeartbeatCache_SnapshotSortedByHandle(t *testing.T) {
	c := NewHeartbeatCache()
	now := time.Now()
	c.Stamp("zulu", "acct-1", `{}`, now)
	c.Stamp("alpha", "acct-1", `{}`, now)
	c.Stamp("mike", "acct-1", `{}`, now)
	got := c.Snapshot()
	handles := []string{got[0].Handle, got[1].Handle, got[2].Handle}
	want := []string{"alpha", "mike", "zulu"}
	if !reflect.DeepEqual(handles, want) {
		t.Errorf("handles = %v, want sorted %v", handles, want)
	}
}

// applyHeartbeatStamp is the helper handleSend / handleSendBatch calls.
// channel == "heartbeat" → stamp; anything else → no-op.
func TestApplyHeartbeatStamp_StampsOnHeartbeatChannel(t *testing.T) {
	c := NewHeartbeatCache()
	now := time.Now()
	applyHeartbeatStamp(c, "heartbeat", "alice", "acct-1", `{"seq":1}`, now)
	if len(c.Snapshot()) != 1 {
		t.Errorf("heartbeat channel did not stamp; snapshot = %v", c.Snapshot())
	}
}

func TestApplyHeartbeatStamp_IgnoresOtherChannels(t *testing.T) {
	c := NewHeartbeatCache()
	now := time.Now()
	for _, ch := range []string{"stdout", "stdin", "stdctrl", "inbox", "broadcast", "custom"} {
		applyHeartbeatStamp(c, ch, "alice", "acct-1", "payload", now)
	}
	if len(c.Snapshot()) != 0 {
		t.Errorf("non-heartbeat channels stamped; snapshot = %v", c.Snapshot())
	}
}

func TestApplyHeartbeatStamp_NilCacheSafe(t *testing.T) {
	// handleSend may be called before the cache is initialised in
	// unusual startup paths or tests. Guarded as a no-op rather than
	// panicking so a missing cache never blocks a publish.
	applyHeartbeatStamp(nil, "heartbeat", "alice", "acct-1", `{"seq":1}`, time.Now())
}
