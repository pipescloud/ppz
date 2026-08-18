package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// Default JetStream stream config for newly-provisioned pipes. Each can
// be overridden per-pipe via the `retention` block on POST /sources/{h}/pipes.
//
// Sizing rationale: stdout publishes ~4 KiB chunks per pty write, so the
// previous 1000-msg cap evicted history after a few seconds of busy TUI
// traffic — long before the 64 MiB byte cap kicked in. Bumping the count
// to 5000 + dropping bytes to 16 MiB makes both caps roughly meet around
// the same point for stdout (4 KiB × 4096 ≈ 16 MiB), and keeps small-
// message pipes (broadcast/stdin/stdctrl) bounded by msg count without
// blowing storage budgets.
const (
	defaultStreamMaxAge   = 24 * time.Hour
	defaultStreamMaxMsgs  = 5000
	defaultStreamMaxBytes = 16 * 1024 * 1024 // 16 MiB
)

// ensurePipeStreamWithRetention provisions the JetStream stream backing one
// (manifold, source, pipe) triple at the given retention. Phase 1.5.1
// manifold-aware, so sources at a non-root manifold provision streams at the
// right wire path.
//
// There is deliberately no defaults-only sibling any more. Since the switch to
// CreateOrUpdateStream (below), provisioning no longer no-ops on an existing
// stream — it RESETS it — so a `provision this pipe at the built-in defaults`
// helper is a loaded gun: called on a pipe carrying a `ppz pipe set` override
// it silently reverts the user's configuration. Callers must resolve retention
// first; ensureSourceStreams does that for a whole source.
func ensurePipeStreamWithRetention(ctx context.Context, js jetstream.JetStream, accountID uuid.UUID, manifold, handle, pipe string, maxAge time.Duration, maxMsgs int, maxBytes int64) error {
	cfg := jetstream.StreamConfig{
		Name:      natsubj.BuildStreamName(accountID, manifold, handle, pipe),
		Subjects:  []string{natsubj.BuildSubject(accountID, manifold, handle, pipe)},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    maxAge,
		MaxMsgs:   int64(maxMsgs),
		MaxBytes:  maxBytes,
		Storage:   jetstream.FileStorage,
		Discard:   jetstream.DiscardOld,
		Replicas:  1,
	}
	// CreateOrUpdate, not Create. The previous Create-and-swallow-
	// ErrStreamNameAlreadyInUse made retention effectively immutable:
	// re-provisioning an existing stream with a different config was a
	// silent no-op, so `ppz pipe set` could update the pipes row and
	// change nothing about what the stream actually retained. It also
	// meant bumping the defaults here never reached streams already in
	// existence.
	if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
		return fmt.Errorf("ensure stream %s: %w", cfg.Name, err)
	}
	return nil
}

// retentionOverride is one layer of the retention resolution chain. A nil
// field means "this layer expresses no opinion" — resolution falls
// through to the next layer down.
type retentionOverride struct {
	TTLSeconds *int
	MaxMsgs    *int
	MaxBytes   *int64
}

// resolveRetention collapses the retention layers into the concrete
// triple provisioned onto a JetStream stream. Layers are passed
// highest-precedence first:
//
//	pipe row override  →  account default  →  built-in default
//
// Fields resolve INDEPENDENTLY: a pipe overriding only max-msgs still
// inherits the account's TTL rather than falling all the way through to
// the built-in. Everything funnels through here so the CLI, the HTTP API
// and the GUI can't grow three different precedence orders — which was
// already half-true, with two open-coded nil-check ladders in
// handlers_api.go alongside account_pool.go's pipeRetention.
//
// The account layer has no storage behind it yet; call sites pass the
// pipe layer only. Adding org-level defaults is then a new layer here,
// not a rewrite at every call site.
func resolveRetention(layers ...retentionOverride) (time.Duration, int, int64) {
	age := defaultStreamMaxAge
	msgs := defaultStreamMaxMsgs
	bytes := int64(defaultStreamMaxBytes)
	for _, l := range layers {
		if l.TTLSeconds != nil {
			age = time.Duration(*l.TTLSeconds) * time.Second
			break
		}
	}
	for _, l := range layers {
		if l.MaxMsgs != nil {
			msgs = *l.MaxMsgs
			break
		}
	}
	for _, l := range layers {
		if l.MaxBytes != nil {
			bytes = *l.MaxBytes
			break
		}
	}
	return age, msgs, bytes
}

// ensureSourceStreams provisions every stream a source needs — its
// kind-derived auto-pipes plus its user-created pipes — with each pipe's
// stored retention override applied.
//
// The override lookup is load-bearing, not an optimisation. `ppz pipe
// set` materialises a `pipes` row for auto-provisioned names too, so
// `inbox` and `stdout` can now carry overrides even though `pipe create`
// can never reach them. Provisioning those through the defaults-only
// ensurePipeStream would reset a configured stream back to
// 24h/5000/16MiB on the next `terminal share` or account re-open, while
// postgres went on reporting the value the user asked for.
//
// Provisioning both sets here also removes an ordering dependency: the
// old caller ran the auto loop before the user loop, and relied on that
// order to un-do its own clobbering.
func ensureSourceStreams(ctx context.Context, pool *db.Pool, js jetstream.JetStream, accountID uuid.UUID, src db.Source) error {
	rows, err := db.ListPipesForSource(ctx, pool, src.ID)
	if err != nil {
		return fmt.Errorf("list pipes for %s: %w", src.Handle, err)
	}
	stored := make(map[string]db.Pipe, len(rows))
	for _, row := range rows {
		stored[row.Name] = row
	}

	done := make(map[string]bool, len(rows)+len(src.Pipes()))
	for _, name := range src.Pipes() {
		age, msgs, bytes := resolveRetention()
		if row, ok := stored[name]; ok {
			age, msgs, bytes = pipeRetention(row)
		}
		if err := ensurePipeStreamWithRetention(ctx, js, accountID, src.Manifold, src.Handle, name, age, msgs, bytes); err != nil {
			return fmt.Errorf("ensure auto stream %s.%s: %w", src.Handle, name, err)
		}
		done[name] = true
	}
	for _, row := range rows {
		if done[row.Name] {
			continue
		}
		age, msgs, bytes := pipeRetention(row)
		if err := ensurePipeStreamWithRetention(ctx, js, accountID, row.Manifold, src.Handle, row.Name, age, msgs, bytes); err != nil {
			return fmt.Errorf("ensure user stream %s.%s: %w", src.Handle, row.Name, err)
		}
	}
	return nil
}

// deletePipeStream removes the JetStream stream backing one (manifold,
// source, pipe). Phase 1.5.1 manifold-aware. Idempotent on missing stream.
func deletePipeStream(ctx context.Context, js jetstream.JetStream, accountID uuid.UUID, manifold, handle, pipe string) error {
	name := natsubj.BuildStreamName(accountID, manifold, handle, pipe)
	if err := js.DeleteStream(ctx, name); err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil
		}
		return fmt.Errorf("delete stream %s: %w", name, err)
	}
	return nil
}

// deleteUncollaredPipeStream removes the JetStream stream backing an
// uncollared pipe (source="" — sourceless). Idempotent. Phase 1.5.
func deleteUncollaredPipeStream(ctx context.Context, js jetstream.JetStream, accountID uuid.UUID, manifold, name string) error {
	streamName := natsubj.BuildStreamName(accountID, manifold, "", name)
	if err := js.DeleteStream(ctx, streamName); err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil
		}
		return fmt.Errorf("delete stream %s: %w", streamName, err)
	}
	return nil
}

// (Phase 1.5.1 collapsed ensurePipeStreamPhase15 into ensurePipeStream
// once the latter became manifold-aware. The two were structurally
// identical after the four-role migration.)

// jsFor returns a JetStream context bound to the server's NATS connection.
func jsFor(nc *nats.Conn) (jetstream.JetStream, error) {
	return jetstream.New(nc)
}
