package server

import (
	"testing"
	"time"

	"github.com/pipescloud/ppz/internal/db"
)

// Retention is about to be settable from more than one place: the pipe
// row (today), an account/org default (next), and eventually the web
// GUI writing either of those. Resolution has to live in ONE function
// with an explicit precedence order, or the CLI, the HTTP API and the
// GUI will each grow their own copy — which is already half-true, since
// handlers_api.go currently open-codes the same nil-check ladder twice
// alongside account_pool.go's pipeRetention.
//
// resolveRetention takes layers highest-precedence first and collapses
// them per-field: the first layer expressing an opinion about a field
// wins, and the server defaults backstop every field.

func TestResolveRetention_NoLayersYieldsServerDefaults(t *testing.T) {
	age, msgs, bytes := resolveRetention()
	if age != defaultStreamMaxAge {
		t.Errorf("maxAge = %v, want %v", age, defaultStreamMaxAge)
	}
	if msgs != defaultStreamMaxMsgs {
		t.Errorf("maxMsgs = %d, want %d", msgs, defaultStreamMaxMsgs)
	}
	if bytes != int64(defaultStreamMaxBytes) {
		t.Errorf("maxBytes = %d, want %d", bytes, defaultStreamMaxBytes)
	}
}

func TestResolveRetention_EmptyLayerDefersToDefaults(t *testing.T) {
	age, msgs, bytes := resolveRetention(retentionOverride{}, retentionOverride{})
	if age != defaultStreamMaxAge || msgs != defaultStreamMaxMsgs || bytes != int64(defaultStreamMaxBytes) {
		t.Errorf("all-nil layers must resolve to defaults, got (%v, %d, %d)", age, msgs, bytes)
	}
}

// Precedence: earlier layer wins. The call sites pass the pipe row
// first, the account default second.
func TestResolveRetention_FirstLayerWins(t *testing.T) {
	pipeTTL, acctTTL := 3600, 7200
	pipeMsgs, acctMsgs := 10, 20
	pipeBytes, acctBytes := int64(1024), int64(2048)

	age, msgs, bytes := resolveRetention(
		retentionOverride{TTLSeconds: &pipeTTL, MaxMsgs: &pipeMsgs, MaxBytes: &pipeBytes},
		retentionOverride{TTLSeconds: &acctTTL, MaxMsgs: &acctMsgs, MaxBytes: &acctBytes},
	)
	if age != time.Hour {
		t.Errorf("maxAge = %v, want 1h (pipe layer wins over account layer)", age)
	}
	if msgs != 10 {
		t.Errorf("maxMsgs = %d, want 10 (pipe layer wins)", msgs)
	}
	if bytes != 1024 {
		t.Errorf("maxBytes = %d, want 1024 (pipe layer wins)", bytes)
	}
}

// Per-field independence is the whole point: a pipe that overrides only
// max-msgs must still inherit the account's TTL, not the account's
// silence-plus-server-default for every other field.
func TestResolveRetention_FieldsResolveIndependently(t *testing.T) {
	pipeMsgs := 42
	acctTTL := 7200
	acctBytes := int64(2048)

	age, msgs, bytes := resolveRetention(
		retentionOverride{MaxMsgs: &pipeMsgs},
		retentionOverride{TTLSeconds: &acctTTL, MaxBytes: &acctBytes},
	)
	if age != 2*time.Hour {
		t.Errorf("maxAge = %v, want 2h (from account layer — pipe is silent)", age)
	}
	if msgs != 42 {
		t.Errorf("maxMsgs = %d, want 42 (from pipe layer)", msgs)
	}
	if bytes != 2048 {
		t.Errorf("maxBytes = %d, want 2048 (from account layer)", bytes)
	}
}

// pipeRetention is the existing entry point (account_pool.go re-provisions
// every stream through it on account boot). It must keep working, and
// must agree with resolveRetention — the two drifting is exactly the bug
// the single resolver exists to prevent.
func TestPipeRetention_DelegatesToResolver(t *testing.T) {
	ttl, msgs := 300, 7
	bytes := int64(999)
	gotAge, gotMsgs, gotBytes := pipeRetention(db.Pipe{TTLSeconds: &ttl, MaxMsgs: &msgs, MaxBytes: &bytes})
	wantAge, wantMsgs, wantBytes := resolveRetention(retentionOverride{TTLSeconds: &ttl, MaxMsgs: &msgs, MaxBytes: &bytes})
	if gotAge != wantAge || gotMsgs != wantMsgs || gotBytes != wantBytes {
		t.Errorf("pipeRetention = (%v, %d, %d), resolveRetention = (%v, %d, %d)",
			gotAge, gotMsgs, gotBytes, wantAge, wantMsgs, wantBytes)
	}
}

func TestPipeRetention_NilsYieldDefaults(t *testing.T) {
	age, msgs, bytes := pipeRetention(db.Pipe{})
	if age != defaultStreamMaxAge || msgs != defaultStreamMaxMsgs || bytes != int64(defaultStreamMaxBytes) {
		t.Errorf("nil overrides must resolve to defaults, got (%v, %d, %d)", age, msgs, bytes)
	}
}
