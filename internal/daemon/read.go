package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/clock"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// handleRead opens a JetStream OrderedConsumer on the pipe's stream, drains
// retained messages applying the requested filters, then either closes (no
// --follow) or keeps streaming live messages until the CLI closes the
// connection (which it does on SIGINT).
//
// Streaming wire format: one JSON ReadEvent per line, followed by the
// daemon closing the socket. The CLI reads ReadEvents until EOF.
func (d *Daemon) handleRead(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.ReadRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeReadErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}

	if _, ok := d.State.Credentials(); !ok {
		writeReadErr(conn, cliproto.New(cliproto.ENotLoggedIn))
		return
	}

	// Phase 1.5: branch on uncollared vs collared. BareTarget != "" =
	// the CLI saw a bare name with no dot; resolve as uncollared at
	// current_namespace. Otherwise: today's collared (handle.pipe) flow.
	uncollared := req.BareTarget != ""
	if uncollared {
		if err := natsubj.ValidatePipe(req.BareTarget); err != nil {
			writeReadErr(conn, cliproto.New(cliproto.EInvalidPipe))
			return
		}
	} else {
		if err := natsubj.ValidatePipe(req.Channel); err != nil {
			writeReadErr(conn, cliproto.New(cliproto.EInvalidPipe))
			return
		}
		if err := natsubj.ValidateHandle(req.Handle); err != nil {
			writeReadErr(conn, cliproto.NewInvalidHandle(req.Handle))
			return
		}

		// Verify the source exists in this org via the server. (Without this any
		// not-yet-created handle would silently return empty rather than error.)
		var listReply cliproto.ListSourcesReply
		if e := d.callServer(ctx, "GET", "/api/v1/sources", nil, &listReply); e != nil {
			writeReadErr(conn, e)
			return
		}
		found := false
		for _, s := range listReply.Sources {
			if s.Handle == req.Handle {
				found = true
				break
			}
		}
		if !found {
			writeReadErr(conn, cliproto.NewSourceNotFound(req.Handle))
			return
		}
	}

	if err := d.ensureNATS(ctx); err != nil {
		writeReadErr(conn, cliproto.New(cliproto.ENATSUnreachable))
		return
	}

	// Register follow conns BEFORE the d.jetStream() capture so a
	// concurrent swapNC (refresh-loop firing on another goroutine)
	// can't slip in between js binding and registry insertion — that
	// window would leave the follow anchored to a stale NC with no
	// eviction path. With registration first, any swap during the
	// drain or consumer-setup phases closes the conn cleanly and the
	// CLI's outer redial loop reconnects.
	if req.Follow && d.Follows != nil {
		d.Follows.add(conn)
		defer d.Follows.remove(conn)
	}

	accountID, err := uuid.Parse(d.State.AccountID())
	if err != nil {
		writeReadErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: "bad account id"})
		return
	}
	var streamName string
	if uncollared {
		manifold := d.State.CurrentNamespace(req.Session)
		streamName = natsubj.BuildStreamName(accountID, manifold, "", req.BareTarget)
	} else {
		streamName = natsubj.BuildStreamName(accountID, d.State.HandleManifold(req.Handle), req.Handle, req.Channel)
	}

	js, e := d.jetStream()
	if e != nil {
		writeReadErr(conn, e)
		return
	}
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		// Classify the error so the user sees the right cause. Mirror of
		// the routing logic in handleSend / resolveSendTarget
		// (handlers.go) — same bug shape, same fix shape:
		//   - jetstream.ErrStreamNotFound → E_PIPE_NOT_FOUND. The pipe
		//     genuinely doesn't exist on this source; message names the
		//     pipe + source so the user sees what's missing.
		//   - network / timeout / no-servers → E_NATS_UNREACHABLE. The
		//     pipe might be perfectly valid; attributing the failure to
		//     pipe invalidity would mislead users into chasing typos
		//     when the real cause is connectivity.
		//   - anything else → E_INVALID_PIPE catch-all. Truly unexpected.
		switch {
		case errors.Is(err, jetstream.ErrStreamNotFound):
			if uncollared {
				manifold := d.State.CurrentNamespace(req.Session)
				writeReadErr(conn, cliproto.NewUncollaredPipeNotFound(req.BareTarget, manifold))
			} else {
				writeReadErr(conn, cliproto.NewPipeNotFound(req.Channel, req.Handle))
			}
		case errors.Is(err, context.DeadlineExceeded),
			errors.Is(err, nats.ErrTimeout),
			errors.Is(err, nats.ErrConnectionClosed),
			errors.Is(err, nats.ErrNoServers):
			if !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrNoServers) {
				d.reportNATSFailure()
			}
			writeReadErr(conn, cliproto.New(cliproto.ENATSUnreachable))
		default:
			writeReadErr(conn, cliproto.New(cliproto.EInvalidPipe))
		}
		return
	}

	// First pass: drain retained messages by GetMsg(seq), apply filters,
	// hold them in a slice so we can apply tail-N at the end.
	info, err := stream.Info(ctx)
	if err != nil {
		writeReadErr(conn, cliproto.New(cliproto.ENATSUnreachable))
		return
	}

	// On stdout reads, emit a leading Meta event with the source pty's
	// current dimensions (sourced from the latest stdctrl resize). The
	// CLI's --tty renderer uses these instead of the hardcoded 200×60
	// default; without it, bytes positioned for a wider source garble
	// when vt10x's grid wraps. Stdctrl missing / unparseable / dims=0
	// are all treated as "no info" — caller falls back to defaults.
	// Uncollared pipes don't have stdctrl semantics (no source identity);
	// skip the meta probe.
	if !uncollared && req.Channel == "stdout" {
		if cols, rows, ok := latestStdctrlResize(ctx, js, accountID, d.State.HandleManifold(req.Handle), req.Handle); ok {
			_ = json.NewEncoder(conn).Encode(cliproto.ReadEvent{
				Meta: &cliproto.ReadMeta{Cols: cols, Rows: rows},
			})
		}
	}

	// Phase 1.5: compute the subject for filter / cursor-key purposes.
	// Uncollared uses the four-role builder; collared uses the legacy
	// three-arg form to keep cursor keys stable.
	var filterSubject string
	if uncollared {
		manifold := d.State.CurrentNamespace(req.Session)
		filterSubject = natsubj.BuildSubject(accountID, manifold, "", req.BareTarget)
	} else {
		filterSubject = natsubj.BuildSubject(accountID, d.State.HandleManifold(req.Handle), req.Handle, req.Channel)
	}

	var (
		retained    []cliproto.ReadMessage
		lastSeqSeen uint64
		moreUnread  int
	)
	// Cursor-aware vs forensic mode. `read` (default) starts at cursor+1 so
	// the agent only sees what's new since they last looked. `reread`
	// (req.All=true) ignores the cursor entirely — replay the full retained
	// stream. NoAdvance is implied by All; cursor never moves under reread.
	cursorKey := daemonCursorKey(accountID, req.Handle, req.Channel)
	if uncollared {
		cursorKey = uncollaredCursorKey(filterSubject)
	}
	startSeq := info.State.FirstSeq
	if !req.All && info.State.Msgs > 0 {
		// effectiveCursor resets the baseline to 0 when the stored cursor
		// was stamped against a prior incarnation of this stream (source
		// destroyed + recreated) or otherwise sits ahead of LastSeq, so a
		// fresh stream reads from its start rather than being silently
		// skipped past.
		entry := d.Cursors.GetEntry(req.Session, cursorKey)
		cursor := effectiveCursor(entry, createdNanos(info.Created), info.State.LastSeq)
		if cursor+1 > startSeq {
			startSeq = cursor + 1
		}
	}
	if info.State.Msgs > 0 && startSeq <= info.State.LastSeq {
		var sinceCutoff time.Time
		if req.SinceMS > 0 {
			sinceCutoff = time.Now().Add(-time.Duration(req.SinceMS) * time.Millisecond)
		}
		// Drain via a single Fetch() on an ephemeral pull consumer
		// instead of N synchronous stream.GetMsg(seq) calls. Fetch
		// pulls the entire window in one batch, so 200 messages take
		// 1 round-trip, not 200. (The per-seq path was catastrophic
		// over WAN: 200ms RTT × N messages = several minutes for a
		// few hundred retained items.)
		//
		// Snapshot the upper bound at request time so a fresh write
		// during drain can't push us past the requested window.
		historicalEnd := info.State.LastSeq
		expected := int(historicalEnd - startSeq + 1)
		consumer, cerr := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
			FilterSubject:     filterSubject,
			DeliverPolicy:     jetstream.DeliverByStartSequencePolicy,
			OptStartSeq:       startSeq,
			AckPolicy:         jetstream.AckNonePolicy,
			InactiveThreshold: 30 * time.Second,
		})
		if cerr != nil {
			writeReadErr(conn, cliproto.New(cliproto.ENATSUnreachable))
			return
		}
		// Best-effort cleanup; the InactiveThreshold catches anything
		// we leak.
		defer func() { _ = stream.DeleteConsumer(ctx, consumer.CachedInfo().Name) }()

		drained := 0
	drainLoop:
		for drained < expected {
			// Head-limit (`read -l`, flood cap): fetch no more than we still
			// intend to deliver, and stop as soon as the quota is met. Unlike
			// the tail-N Limit below (a post-drain slice for `reread`), the
			// cap must bound the drain itself so lastSeqSeen — and therefore
			// the cursor advance — stops at the last DELIVERED message; the
			// remainder stays unread and pages out on the next invocation.
			want := expected - drained
			if req.HeadLimit > 0 {
				if room := req.HeadLimit - len(retained); room < want {
					want = room
				}
				if want <= 0 {
					break
				}
			}
			batch, ferr := consumer.Fetch(want, jetstream.FetchMaxWait(5*time.Second))
			if ferr != nil {
				break
			}
			any := false
			for msg := range batch.Messages() {
				any = true
				md, mderr := msg.Metadata()
				if mderr != nil {
					drained++
					continue
				}
				env, eerr := envelope.Unmarshal(msg.Data())
				if eerr == nil && (sinceCutoff.IsZero() || !env.CreatedAt.Before(sinceCutoff)) {
					retained = append(retained, cliproto.ReadMessage{
						ID:           env.ID,
						Sender:       env.Sender,
						Subject:      env.Subject,
						Payload:      env.Payload,
						CreatedAt:    env.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
						InReplyTo:    env.InReplyTo,
						AckRequested: env.AckRequested,
						ScheduleID:   env.ScheduleID,
					})
					lastSeqSeen = md.Sequence.Stream
					if req.HeadLimit > 0 && len(retained) >= req.HeadLimit {
						drained++
						break drainLoop
					}
				}
				drained++
			}
			if err := batch.Error(); err != nil {
				break
			}
			if !any {
				break
			}
		}
		// Truncated by the head cap? Report what's left so the CLI can
		// render the "(N more unread)" trailer. Sequences in a per-pipe
		// stream are contiguous within the retained window, so the
		// arithmetic is exact.
		if req.HeadLimit > 0 && len(retained) >= req.HeadLimit && lastSeqSeen < historicalEnd {
			moreUnread = int(historicalEnd - lastSeqSeen)
		}
	}
	if req.Skip > 0 && req.Skip < len(retained) {
		retained = retained[req.Skip:]
	} else if req.Skip >= len(retained) {
		retained = nil
	}
	if req.Limit > 0 && req.Limit < len(retained) {
		retained = retained[len(retained)-req.Limit:]
	}

	enc := json.NewEncoder(conn)
	for _, m := range retained {
		mm := m
		if err := enc.Encode(cliproto.ReadEvent{Message: &mm}); err != nil {
			return
		}
	}
	if moreUnread > 0 {
		if err := enc.Encode(cliproto.ReadEvent{MoreUnread: &moreUnread}); err != nil {
			return
		}
	}

	// Advance the session's cursor to whatever we just delivered. Skipped
	// when the caller passed NoAdvance (e.g. `ppz terminal view`, which is
	// observational and shouldn't change anyone's unread count) or All (the
	// `reread` forensic verb leaves cursor state untouched by design).
	if !req.NoAdvance && !req.All && lastSeqSeen > 0 {
		_ = d.Cursors.Advance(req.Session, cursorKey, lastSeqSeen, createdNanos(info.Created))

		// Auto-emit `ack:read` back to each original sender whose
		// message had AckRequested set (v0.25.0 §4). Advance-then-emit
		// is intentional and load-bearing: the cursor must move
		// regardless of whether ack publishing succeeds, so a NATS
		// partition can't wedge reading.
		//
		// Detached on a goroutine so the read RPC returns to the CLI as
		// soon as the cursor has been written. publishEnvelope's Flush
		// is a NATS round-trip; under WAN that's tens or hundreds of ms
		// per ack. With N ack-eligible messages drained, a synchronous
		// loop would hold the read open for N×RTT. emitAcks itself is
		// designed to be best-effort (failures are silently dropped),
		// so detaching is safe — at most we lose acks that would have
		// been lost anyway under the same failure conditions.
		//
		// Reread / NoAdvance reads do NOT emit acks — the cursor isn't
		// moving from the recipient's perspective, so claiming "they
		// read it" would be wrong.
		go emitAcks(accountID, senderForRequest(req.Sender, d.State.Current(req.Session)), retained, clock.Now(), d.publishEnvelope)
	}

	if !req.Follow {
		return
	}

	// Follow conn was already registered up top (before js capture)
	// so swapNC can evict it cleanly. Pinned by the share-stdin- and
	// share-inbox-alerts-survives-share-daemon-{logout,relogin,
	// restart} e2e tests.
	//
	// Follow mode: open a live consumer starting just after the last
	// sequence we drained (so we don't double-deliver). Stream until the
	// CLI closes the socket or the request ctx is cancelled.
	consumer, err := stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{filterSubject},
		DeliverPolicy:  jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:    lastSeqSeen + 1,
	})
	if err != nil {
		return
	}

	cctx, err := consumer.Consume(func(msg jetstream.Msg) {
		env, err := envelope.Unmarshal(msg.Data())
		if err != nil {
			_ = msg.Ack()
			return
		}
		rm := cliproto.ReadMessage{
			ID:           env.ID,
			Sender:       env.Sender,
			Subject:      env.Subject,
			Payload:      env.Payload,
			CreatedAt:    env.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			InReplyTo:    env.InReplyTo,
			AckRequested: env.AckRequested,
			ScheduleID:   env.ScheduleID,
		}
		if err := enc.Encode(cliproto.ReadEvent{Message: &rm}); err != nil {
			// CLI has closed the connection — tear down.
			return
		}
		// Advance the session's cursor as live messages stream by, so a
		// long-running follow keeps the unread count truthful.
		if !req.NoAdvance && !req.All {
			if md, mderr := msg.Metadata(); mderr == nil {
				_ = d.Cursors.Advance(req.Session, cursorKey, md.Sequence.Stream, createdNanos(info.Created))
			}
			// Per-message ack auto-emit for live messages (v0.25.0 §4).
			// Detached: synchronous emission inside the Consume callback
			// would apply NATS-level backpressure (the next message waits
			// for the previous ack's Flush), chunking the live stream by
			// per-ack RTT. Same fire-and-forget semantics as the
			// historical-drain path above.
			go emitAcks(accountID, senderForRequest(req.Sender, d.State.Current(req.Session)), []cliproto.ReadMessage{rm}, clock.Now(), d.publishEnvelope)
		}
		_ = msg.Ack()
	})
	if err != nil {
		return
	}
	defer cctx.Stop()

	// Block until the CLI closes the socket. We detect that by reading from
	// the connection — the CLI never writes after the initial request, so a
	// non-zero read or EOF means the client went away. conn.Read also
	// unblocks when closeAll() closes the conn during a swapNC (e.g. JWT
	// rotation), so this fires on both client-gone and daemon-side eviction.
	done := make(chan struct{})
	go func() {
		var b [1]byte
		_, _ = conn.Read(b[:])
		cctx.Stop()
		close(done)
	}()

	// Exit when the follow conn is closed (client-gone or closeAll) or the
	// daemon shuts down. Waiting solely on <-ctx.Done() would leak this
	// goroutine on every NC swap, since ctx only cancels at daemon shutdown.
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func writeReadErr(conn net.Conn, e *cliproto.Error) {
	_ = json.NewEncoder(conn).Encode(cliproto.ReadEvent{Error: e})
}

// parseTarget splits "handle.channel" or returns an error if the form is
// wrong. Bare "handle" (no dot) is rejected explicitly per Phase 1 spec.
func parseTarget(target string) (handle, channel string, err error) {
	idx := strings.LastIndex(target, ".")
	if idx <= 0 || idx == len(target)-1 {
		return "", "", errors.New("target must be <handle>.<channel>")
	}
	return target[:idx], target[idx+1:], nil
}

// stdctrlResizePayload mirrors the {type:"resize",cols,rows} JSON
// `terminal share` publishes via publishWinsize. Local to read.go since
// nothing else needs to deserialise these — the server does its own
// inline parse for the GUI WebSocket path.
type stdctrlResizePayload struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// latestStdctrlResize fetches the most recent resize event from a
// source's stdctrl pipe and returns its dimensions. Used by handleRead
// to emit a leading Meta event on `<h>.stdout` reads so the CLI can
// configure its --tty renderer at the source's actual size instead of
// the hardcoded 200×60 default.
//
// Returns (0, 0, false) when:
//   - the stdctrl stream doesn't exist (handle never had `terminal share`),
//   - the stream is empty, or
//   - the latest message isn't a parseable resize event.
//
// All of those collapse to "no Meta — fall back to defaults". Sane and
// boring; the caller doesn't need to distinguish.
//
// Dimensions are clamped to [1, 1000] on each axis so a malformed or
// runaway publisher can't pass nonsense to vt10x.
func latestStdctrlResize(ctx context.Context, js jetstream.JetStream, accountID uuid.UUID, manifold, handle string) (cols, rows int, ok bool) {
	streamName := natsubj.BuildStreamName(accountID, manifold, handle, "stdctrl")
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		return 0, 0, false
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs == 0 {
		return 0, 0, false
	}
	msg, err := stream.GetMsg(ctx, info.State.LastSeq)
	if err != nil {
		return 0, 0, false
	}
	env, err := envelope.Unmarshal(msg.Data)
	if err != nil {
		return 0, 0, false
	}
	var rs stdctrlResizePayload
	if err := json.Unmarshal([]byte(env.Payload), &rs); err != nil || rs.Type != "resize" {
		return 0, 0, false
	}
	if rs.Cols <= 0 || rs.Rows <= 0 {
		return 0, 0, false
	}
	if rs.Cols > 1000 {
		rs.Cols = 1000
	}
	if rs.Rows > 1000 {
		rs.Rows = 1000
	}
	return rs.Cols, rs.Rows, true
}
