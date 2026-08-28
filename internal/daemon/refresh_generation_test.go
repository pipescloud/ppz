package daemon

import (
	"context"
	"testing"
	"time"
)

// JWTExp is unix SECONDS. Two refreshes landing in the same second mint
// credentials with an identical exp, so a caller coalescing on exp alone
// concludes it is already on the current generation and skips the
// redial — holding a freshly-minted credential it never dials with.
//
// That was harmless while every credential carried the same
// permissions. Once ACL enforcement (Phase 3) lets a refresh NARROW
// access, the skipped redial means a principal keeps using access it no
// longer has, which is precisely the case
// tests/acl/enforce-toggle-restricts-then-restores exercises.
func TestRefreshLoop_GenerationAdvancesWithinTheSameSecond(t *testing.T) {
	sameExp := time.Now().Add(5 * time.Minute).Unix()
	r := &RefreshLoop{
		AccountID: "acct",
		Refresh: func(context.Context, string) (string, string, int64, error) {
			return "jwt", "seed", sameExp, nil
		},
	}

	if got := r.Generation(); got != 0 {
		t.Fatalf("fresh loop generation = %d, want 0", got)
	}
	for i := 1; i <= 3; i++ {
		if err := r.ForceRefresh(context.Background()); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
		if got := r.Generation(); got != uint64(i) {
			t.Errorf("after refresh %d: generation = %d, want %d", i, got, i)
		}
	}
	// The exp never moved — which is exactly why exp cannot be the
	// generation signal on its own.
	if r.JWTExp() != sameExp {
		t.Fatalf("control: exp should be unchanged across the refreshes, got %d want %d", r.JWTExp(), sameExp)
	}
}

// A nil loop is the pre-login state; callers must not have to nil-check.
func TestRefreshLoop_GenerationNilSafe(t *testing.T) {
	var r *RefreshLoop
	if got := r.Generation(); got != 0 {
		t.Errorf("nil loop generation = %d, want 0", got)
	}
}
