package daemon

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// Retention needs NO new server endpoint and no extra round trip: the
// daemon already holds a *jetstream.StreamInfo per pipe (that is where
// BUFFERED and LAST come from), and StreamConfig carries the three caps
// alongside the state it reads today.
//
// Reading them from JetStream rather than the `pipes` table also gets
// auto-provisioned pipes right. `inbox` and `stdout` have no row until
// someone runs `pipe set`, so a DB-sourced answer would be blank for
// exactly the pipes whose caps users hit first — while JetStream always
// knows what the stream is actually enforcing.
func TestApplyRetention_CopiesCapsFromStreamConfig(t *testing.T) {
	var info cliproto.PipeInfo
	applyRetention(&info, jetstream.StreamConfig{
		MaxAge:   24 * time.Hour,
		MaxMsgs:  5000,
		MaxBytes: 16 * 1024 * 1024,
	})

	if info.TTLSeconds != 86400 {
		t.Errorf("TTLSeconds = %d, want 86400", info.TTLSeconds)
	}
	if info.MaxMsgs != 5000 {
		t.Errorf("MaxMsgs = %d, want 5000", info.MaxMsgs)
	}
	if info.MaxBytes != 16*1024*1024 {
		t.Errorf("MaxBytes = %d, want %d", info.MaxBytes, 16*1024*1024)
	}
}

// JetStream spells "no limit" as -1, not 0. Copying that through as-is
// would render "-1" in a column users read as a number; it means
// unlimited, and the formatter needs to be able to tell it apart from a
// real cap.
func TestApplyRetention_PreservesUnlimitedSentinel(t *testing.T) {
	var info cliproto.PipeInfo
	applyRetention(&info, jetstream.StreamConfig{MaxAge: 0, MaxMsgs: -1, MaxBytes: -1})

	if info.MaxMsgs != -1 {
		t.Errorf("MaxMsgs = %d, want -1 (unlimited preserved, not flattened to 0)", info.MaxMsgs)
	}
	if info.MaxBytes != -1 {
		t.Errorf("MaxBytes = %d, want -1 (unlimited preserved)", info.MaxBytes)
	}
	if info.TTLSeconds != 0 {
		t.Errorf("TTLSeconds = %d, want 0 (MaxAge 0 is 'no age limit')", info.TTLSeconds)
	}
}

// Sub-second MaxAge truncates rather than rounding to zero — a pipe with
// a 1500ms age limit should not read as "no TTL".
func TestApplyRetention_TruncatesSubSecondMaxAge(t *testing.T) {
	var info cliproto.PipeInfo
	applyRetention(&info, jetstream.StreamConfig{MaxAge: 1500 * time.Millisecond})
	if info.TTLSeconds != 1 {
		t.Errorf("TTLSeconds = %d, want 1", info.TTLSeconds)
	}
}
