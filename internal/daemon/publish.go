package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// jsPublishAckTimeout bounds how long the daemon waits for a JetStream
// PubAck before declaring the delivery unconfirmed. Per the send
// delivery contract, a publish is a success ONLY when the server has
// acknowledged durable storage — so we wait for the ack rather than
// trusting a core NC.Flush, which confirms only that the bytes reached
// the NATS core (not that JetStream persisted them — the Bug B
// silent-loss root cause: across a reconnect window a core publish can
// Flush "ok" yet never durably land).
var jsPublishAckTimeout = 5 * time.Second

// publishWithAck publishes data to subject and blocks for the JetStream
// PubAck, bounded by jsPublishAckTimeout. It is the single ack'd publish
// primitive behind every send path. Returns nil only on a confirmed
// PubAck; otherwise a classified *cliproto.Error.
func (d *Daemon) publishWithAck(subject string, data []byte) *cliproto.Error {
	js, err := jetstream.New(d.NC)
	if err != nil {
		return cliproto.New(cliproto.ENATSUnreachable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), jsPublishAckTimeout)
	defer cancel()
	if _, err := js.Publish(ctx, subject, data); err != nil {
		return classifyPublishErr(err)
	}
	return nil
}

// publishBatchWithAck publishes N data payloads to the SAME subject via
// JetStream async, then BLOCKS for a single batched wait covering every
// PubAck, bounded by jsPublishAckTimeout. Amortises the per-ack latency
// across the batch (preserving the throughput shape of the legacy core
// Publish+single-Flush) while still confirming durable delivery of
// every message (contract clause 1). A missing or failed ack for ANY
// message fails the whole batch; partial delivery is never reported as
// success.
func (d *Daemon) publishBatchWithAck(subject string, datas [][]byte) *cliproto.Error {
	js, err := jetstream.New(d.NC)
	if err != nil {
		return cliproto.New(cliproto.ENATSUnreachable)
	}
	futures := make([]jetstream.PubAckFuture, 0, len(datas))
	for _, data := range datas {
		f, perr := js.PublishAsync(subject, data)
		if perr != nil {
			return classifyPublishErr(perr)
		}
		futures = append(futures, f)
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(jsPublishAckTimeout):
		return cliproto.New(cliproto.EDeliveryUnconfirmed)
	}
	for _, f := range futures {
		select {
		case <-f.Ok():
		case e := <-f.Err():
			return classifyPublishErr(e)
		}
	}
	return nil
}

// classifyPublishErr maps a publish/ack failure to the user-facing code:
//   - connection unusable (closed / no servers) → E_NATS_UNREACHABLE
//     (the message provably never left the client), and
//   - everything else (ack timeout, no stream responded) →
//     E_DELIVERY_UNCONFIRMED (no PubAck — may or may not have landed).
func classifyPublishErr(err error) *cliproto.Error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, nats.ErrConnectionClosed), errors.Is(err, nats.ErrNoServers):
		return cliproto.New(cliproto.ENATSUnreachable)
	default:
		return cliproto.New(cliproto.EDeliveryUnconfirmed)
	}
}

// buildBroadcastEnvelope is the pure envelope-assembly step inside
// handleSend. Kept separate so unit tests can verify field plumbing
// (sender, subject, in_reply_to, ack_requested) without standing up NATS
// or the daemon's IPC plumbing.
//
// `sender` is the broadcaster's *own* current source — distinct from the
// request's Handle, which is the destination. Empty string is permitted
// (headless publish).
func buildBroadcastEnvelope(req cliproto.SendRequest, sender string, now time.Time) envelope.Message {
	env := envelope.New(sender, req.MsgSubject, req.Payload, now)
	env.InReplyTo = req.InReplyTo
	env.AckRequested = req.AckRequested
	return env
}

// publishEnvelope is the in-process helper for daemon-internal envelope
// emission. Two callers:
//
//   - handleSend (after assembling the envelope from a CLI request)
//   - the read-path ack auto-emitter (which builds an `ack:read` envelope
//     directly and bypasses handleSend on purpose, since the IPC-
//     boundary `ack:` rejection rule has no exception path).
//
// No validation here — the rule is "validate at the IPC trust boundary
// (handleSend), trust the daemon-internal callers". Returns nil on
// successful publish + flush.
func (d *Daemon) publishEnvelope(accountID uuid.UUID, dest, pipe string, env envelope.Message) error {
	data, err := env.Marshal()
	if err != nil {
		return err
	}
	// Phase 1.5.1: look up the destination handle's manifold so the
	// subject lands at the right path. Empty for handles cached at
	// root (the common case).
	subject := natsubj.BuildSubject(accountID, d.State.HandleManifold(dest), dest, pipe)
	// Wait for the JetStream PubAck — a confirmed durable write — rather
	// than a core Flush. Returning the typed error as a plain error is
	// safe: publishWithAck returns a nil *cliproto.Error, and we convert
	// via an explicit nil check (avoiding the typed-nil-interface trap).
	if e := d.publishWithAck(subject, data); e != nil {
		return e
	}
	return nil
}
