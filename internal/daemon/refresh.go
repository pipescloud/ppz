package daemon

// Phase 3.5 — daemon JWT refresh loop. Watches a cached User JWT's
// `exp` claim and re-runs /auth/exchange before it expires, swapping
// the cached (jwt, seed) atomically so the next NATS reconnect picks
// up the new credentials.

import (
	"context"
	"errors"
	"sync"
	"time"
)

// RefreshFn is the work the refresh loop calls when a JWT is about
// to expire. Implementations re-run POST /api/v1/auth/exchange and
// return the new (jwt, seed). accountID lets a multi-org daemon route
// to the right account.
type RefreshFn func(ctx context.Context, accountID string) (jwt, seed string, expUnix int64, err error)

// ErrUnauthorized is what RefreshFn returns when the bearer was
// revoked / expired — distinct from transient network failures
// (which the loop retries). Triggers OnUnauthorized + stop.
var ErrUnauthorized = errors.New("daemon.RefreshLoop: unauthorized")

// skewSeconds is how far before exp we re-run /auth/exchange. The
// User JWT's `nbf` claim is also set 30s before now on the server
// side, so a 30s skew here means rotation happens with a 60s
// overlap window where both old + new JWTs are valid.
const skewSeconds = 30

// retryAfter is the backoff between retries on transient (non-401)
// errors from RefreshFn.
const retryAfter = 5 * time.Second

// refreshErrorCoalesceWindow rate-limits refresh_error event recording
// for an UNCHANGED failure reason (see startRefreshLoop's OnError
// hook). One event per window keeps a multi-hour outage within the
// ring (256 entries ≈ 4h at this rate) instead of letting per-retry
// spam evict the outage's first events.
const refreshErrorCoalesceWindow = time.Minute

// OnRefreshedFn fires after refreshNow successfully swaps in fresh
// credentials. The daemon hooks this to proactively rebuild its NATS
// connection within the 60s overlap window (User JWT `nbf` is set 30s
// before issuance, so old + new are both valid for ~60s around the
// rotation point), eliminating the disconnect/reconnect gap that
// otherwise lands when the server kicks the live connection at the
// old JWT's exp — and the transient E_NATS_UNREACHABLE that a send
// running inside that gap surfaces to callers.
//
// Runs synchronously on the refresh goroutine. Implementations must
// not block on anything that could deadlock with the goroutine that
// called RefreshNowIfDue.
type OnRefreshedFn func(jwt, seed string, expUnix int64)

// RefreshLoop monitors one (org, JWT) pair and refreshes it before
// expiry. Concurrency: Current() may be called from any goroutine;
// Start/Stop must be called from the same goroutine.
type RefreshLoop struct {
	AccountID      string
	Refresh        RefreshFn
	OnUnauthorized func(accountID string)
	OnRefreshed    OnRefreshedFn

	// OnError fires on EVERY failed refresh attempt (including the
	// terminal ErrUnauthorized, immediately before OnUnauthorized) with
	// the underlying error. The daemon hooks this to record a
	// "refresh_error" event — the 2026-06-11 incident bundle was
	// undiagnosable precisely because failed /auth/exchange attempts
	// left no trace (ensureNATS collapses them all to
	// E_SERVER_UNREACHABLE). Runs synchronously on whichever goroutine
	// attempted the refresh (the loop, RefreshNowIfDue callers, …)
	// with no RefreshLoop lock held; implementations must be cheap and
	// must not call back into RefreshLoop.
	OnError func(err error)

	mu      sync.Mutex
	jwt     string
	seed    string
	expUnix int64
	lastAt  time.Time
	gen     uint64
	cancel  context.CancelFunc
}

// Start begins the refresh goroutine with an initial credential.
// expUnix is the JWT's `exp` claim in unix seconds.
func (r *RefreshLoop) Start(ctx context.Context, jwt, seed string, expUnix int64) error {
	if r.Refresh == nil {
		return errors.New("RefreshLoop.Start: Refresh fn is required")
	}
	loopCtx, cancel := context.WithCancel(ctx)

	r.mu.Lock()
	r.jwt = jwt
	r.seed = seed
	r.expUnix = expUnix
	r.lastAt = time.Now()
	r.cancel = cancel
	r.mu.Unlock()

	go r.run(loopCtx)
	return nil
}

// Stop halts the refresh goroutine. Idempotent.
func (r *RefreshLoop) Stop() {
	r.mu.Lock()
	c := r.cancel
	r.mu.Unlock()
	if c != nil {
		c() // context.CancelFunc is documented as safe to call repeatedly
	}
}

// Current returns the freshest (jwt, seed) — used by nats.UserJWT()
// callbacks on every NATS (re)connect.
func (r *RefreshLoop) Current() (jwt, seed string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jwt, r.seed
}

// LastRefreshAt returns when the loop last accepted fresh credentials.
// Start counts as the first refresh because its credentials came from
// /auth/exchange immediately before the loop was started.
func (r *RefreshLoop) LastRefreshAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastAt
}

// JWTExp returns the unix-seconds `exp` claim of the cached JWT, or 0
// if no credentials have been cached yet. Used by natsObserveOptions
// to stamp the JWT exp onto every connection-state event — the
// post-rotation-auth-violation pattern relies on this to correlate
// disconnects with rotation timing. Safe for concurrent callers; nil-
// receiver returns 0 so callers can pass r.JWTExp without nil-checking.
// Generation counts successful refreshes. It exists because JWTExp is
// unix SECONDS: two refreshes landing in the same second mint
// credentials with an identical exp, and a caller coalescing on exp
// alone concludes it is already on the current generation and skips the
// redial. The freshly-minted credential is then held but never used.
//
// Harmless while every credential carries the same permissions; once
// ACL enforcement (Phase 3) makes a refresh able to NARROW access, the
// skipped redial means a principal keeps using access it no longer has.
func (r *RefreshLoop) Generation() uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gen
}

func (r *RefreshLoop) JWTExp() int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expUnix
}

// RefreshNowIfDue refreshes synchronously when the cached credential is already
// inside its refresh window. It covers machines waking from sleep: the timer
// goroutine may not have run yet, but the next command must not continue with
// an expired JWT.
func (r *RefreshLoop) RefreshNowIfDue(ctx context.Context, now time.Time) (bool, error) {
	r.mu.Lock()
	exp := r.expUnix
	r.mu.Unlock()

	fireAt := time.Unix(exp, 0).Add(-time.Duration(skewSeconds) * time.Second)
	if now.Before(fireAt) {
		return false, nil
	}
	if err := r.refreshNow(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (r *RefreshLoop) run(ctx context.Context) {
	for {
		// Sleep until exp - skew, with a small floor so we never
		// busy-loop if the supplied JWT is already past its skew
		// window (unit tests pass JWTs that expire in <1s).
		r.mu.Lock()
		exp := r.expUnix
		r.mu.Unlock()

		fireAt := time.Unix(exp, 0).Add(-time.Duration(skewSeconds) * time.Second)
		delay := time.Until(fireAt)
		if delay < 100*time.Millisecond {
			delay = 100 * time.Millisecond
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		if err := r.refreshNow(ctx); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				return
			}
			// Transient — back off and retry. The cached creds are
			// still the previous (still-valid until exp) values, so
			// callers' nats.UserJWT callback continues working until
			// the next reconnect.
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryAfter):
				continue
			}
		}
	}
}

// ForceRefresh re-exchanges immediately, regardless of how much of the
// credential's lifetime is left, and fires OnRefreshed so the caller
// rebuilds its NATS connection.
//
// Used when the server signals that access changed (ACL Phase 3). NATS
// evaluates permissions only at connect, so a grant, revoke or
// enforcement toggle does not reach a live connection until it redials
// with a freshly minted credential.
func (r *RefreshLoop) ForceRefresh(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return r.refreshNow(ctx)
}

func (r *RefreshLoop) refreshNow(ctx context.Context) error {
	newJWT, newSeed, newExp, err := r.Refresh(ctx, r.AccountID)
	if err != nil && r.OnError != nil {
		r.OnError(err)
	}
	if errors.Is(err, ErrUnauthorized) {
		if r.OnUnauthorized != nil {
			r.OnUnauthorized(r.AccountID)
		}
		return err
	}
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.jwt = newJWT
	r.seed = newSeed
	r.expUnix = newExp
	r.gen++
	r.lastAt = time.Now()
	// Capture under lock; invoke without it so callers (the daemon's
	// swapNC) can't deadlock with Current() / other RefreshLoop methods.
	cb := r.OnRefreshed
	r.mu.Unlock()

	if cb != nil {
		cb(newJWT, newSeed, newExp)
	}
	return nil
}
