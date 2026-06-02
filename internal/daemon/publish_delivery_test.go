package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// startEmbeddedJS spins up an in-process NATS server with JetStream
// enabled (no auth — test-only) and returns a connected client. Mirrors
// the embedded-server pattern in internal/natsauth/*_integration_test.go
// but without the JWT machinery, since these tests only exercise the
// publish/delivery seam.
func startEmbeddedJS(t *testing.T) *nats.Conn {
	t.Helper()
	s, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // ephemeral
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new embedded nats: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		s.Shutdown()
		t.Fatalf("embedded nats not ready")
	}
	t.Cleanup(s.Shutdown)

	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect embedded nats: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// TestPublishEnvelope_MustErrorWhenMessageIsNotDurablyStored pins the
// delivery contract at the daemon's publish primitive:
//
//	a publish that the CLI reports as success (exit 0, `sent id=…`) must
//	mean the message is on the server — so publishEnvelope must NOT
//	return nil for a message that was never durably stored.
//
// publishEnvelope currently does core `NC.Publish` + `NC.Flush`. Flush
// only confirms the bytes reached the NATS *core* server, NOT that
// JetStream persisted them — so a publish to a subject no stream
// captures is silently dropped while Publish and Flush both return nil.
// That is exactly the production "exit 0 but the message never landed"
// report (confirmed: the message never reached the server).
//
// RED: with core publish + flush, publishEnvelope returns nil here even
// though nothing stored the message.
// GREEN: a JetStream publish-with-ack (js.Publish / PublishMsg + context
// timeout) returns an error when no PubAck arrives, so publishEnvelope
// surfaces the non-delivery instead of masking it.
func TestPublishEnvelope_MustErrorWhenMessageIsNotDurablyStored(t *testing.T) {
	nc := startEmbeddedJS(t)
	d := New(t.TempDir(), "")
	d.NC = nc

	// Deliberately create NO stream — whatever subject publishEnvelope
	// derives, nothing on the server will persist it. A correct,
	// delivery-confirming publish must report this as a failure.
	env := envelope.New("alice", "", "hello while undeliverable", time.Now())
	err := d.publishEnvelope(uuid.New(), "bob", "inbox", env)

	if err == nil {
		t.Fatalf("publishEnvelope returned nil, but no JetStream stream captured the subject so the message was silently dropped; " +
			"the delivery contract requires an error (publish must wait for a JetStream PubAck, not just Flush the core connection)")
	}
}

// TestPublishEnvelope_SucceedsAndPersistsWhenStreamCaptures is the
// positive control: when a stream DOES capture the subject, the publish
// must both report success AND be retrievable from the server. This
// guards against a future over-correction where publishEnvelope starts
// erroring on the happy path, and proves the test harness (embedded JS,
// subject derivation) is wired correctly.
func TestPublishEnvelope_SucceedsAndPersistsWhenStreamCaptures(t *testing.T) {
	nc := startEmbeddedJS(t)
	d := New(t.TempDir(), "")
	d.NC = nc

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	// Bind a stream to the exact subject publishEnvelope will derive for
	// this account/handle/pipe, so the message has somewhere to land.
	accountID := uuid.New()
	subject := natsubj.BuildSubject(accountID, d.State.HandleManifold("bob"), "bob", "inbox")
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "INBOX",
		Subjects: []string{subject},
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}

	env := envelope.New("alice", "", "hello delivered", time.Now())
	if err := d.publishEnvelope(accountID, "bob", "inbox", env); err != nil {
		t.Fatalf("publishEnvelope to a captured subject errored: %v", err)
	}

	// Independently confirm the message is actually on the server.
	stream, err := js.Stream(ctx, "INBOX")
	if err != nil {
		t.Fatalf("lookup stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Fatalf("stream stored %d messages, want 1 — publishEnvelope reported success but the message is not on the server", info.State.Msgs)
	}
}

// TestPublishBatchWithAck_MustErrorWhenStreamMissing mirrors the single-
// send contract guard for the batch path. handleSendBatch publishes N
// messages via PublishAsync and waits for a batched ack — the same
// success-only-on-PubAck invariant must hold, or a batch could report
// success while one or more messages never durably landed.
func TestPublishBatchWithAck_MustErrorWhenStreamMissing(t *testing.T) {
	nc := startEmbeddedJS(t)
	d := New(t.TempDir(), "")
	d.NC = nc

	accountID := uuid.New()
	subject := natsubj.BuildSubject(accountID, d.State.HandleManifold("bob"), "bob", "inbox")
	datas := [][]byte{[]byte("a"), []byte("b"), []byte("c")}

	if e := d.publishBatchWithAck(subject, datas); e == nil {
		t.Fatalf("publishBatchWithAck returned nil, but no JetStream stream captured the subject so every message was silently dropped; " +
			"the delivery contract requires an error on any unconfirmed publish")
	}
}

// TestPublishBatchWithAck_SucceedsAndPersistsAllWhenStreamCaptures is
// the positive control for the batch path: with a stream bound, the
// helper must report success AND every message must be retrievable from
// the server (not a prefix, not "at least one"). Guards against an
// over-correction where PublishAsyncComplete fires before all futures
// resolve or a future error is missed.
func TestPublishBatchWithAck_SucceedsAndPersistsAllWhenStreamCaptures(t *testing.T) {
	nc := startEmbeddedJS(t)
	d := New(t.TempDir(), "")
	d.NC = nc

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	accountID := uuid.New()
	subject := natsubj.BuildSubject(accountID, d.State.HandleManifold("bob"), "bob", "inbox")
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "INBOX",
		Subjects: []string{subject},
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}

	datas := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
	if e := d.publishBatchWithAck(subject, datas); e != nil {
		t.Fatalf("publishBatchWithAck to a captured subject errored: %v", e)
	}

	stream, err := js.Stream(ctx, "INBOX")
	if err != nil {
		t.Fatalf("lookup stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Msgs != uint64(len(datas)) {
		t.Fatalf("stream stored %d messages, want %d — publishBatchWithAck reported success but not every message is on the server",
			info.State.Msgs, len(datas))
	}
}
