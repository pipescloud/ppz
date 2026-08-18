package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

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

// ensurePipeStream creates a JetStream stream backing one (manifold, source, pipe)
// triple with the default retention. Phase 1.5.1: manifold parameter
// added so sources at non-root manifold provision streams at the right
// wire path. Idempotent on duplicate creation.
func ensurePipeStream(ctx context.Context, js jetstream.JetStream, accountID uuid.UUID, manifold, handle, pipe string) error {
	return ensurePipeStreamWithRetention(ctx, js, accountID, manifold, handle, pipe,
		defaultStreamMaxAge, defaultStreamMaxMsgs, defaultStreamMaxBytes)
}

// ensurePipeStreamWithRetention is the override-aware variant. Used by
// `pipe create` to honour --ttl / --max-msgs / --max-bytes flags.
// Phase 1.5.1 manifold-aware.
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
