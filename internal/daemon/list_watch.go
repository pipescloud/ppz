package daemon

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// handleListWatch is `ppz ls --watch [pattern...]`. Level-triggered: if
// the calling session already has unread on any pipe whose handle
// matches one of the patterns, it returns immediately. Otherwise it
// subscribes to the org's NATS subject space, blocks until a matching
// publish lands, then re-snapshots and returns.
//
// Patterns are filepath.Match-style globs against the handle (not
// handle.pipe). Empty Patterns slice means "any handle".
//
// Single response, not a stream. The CLI prints once and exits — same
// agent-loop pattern as `read` (without `--tail`).
func (d *Daemon) handleListWatch(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.ListWatchRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	if _, ok := d.State.Credentials(); !ok {
		writeIPCErr(conn, cliproto.New(cliproto.ENotLoggedIn))
		return
	}
	accountID, err := uuid.Parse(d.State.AccountID())
	if err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: "bad org id"})
		return
	}
	if err := d.ensureNATS(ctx); err != nil {
		if e, ok := err.(*cliproto.Error); ok {
			writeIPCErr(conn, e)
		} else {
			writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		}
		return
	}

	// Subscribe BEFORE the initial snapshot so a message landing between
	// "build snapshot" and "subscribe" can't make us hang on a state we
	// just observed as caught-up. NATS sub buffers in the client; we
	// drain into a 1-slot wakeup channel which is enough for "tell me
	// when something happened" semantics.
	//
	// armWatch registers the sub with d.Watches so a swapNCLocked rearms
	// it onto the new conn — without that, a JWT-refresh swap (~10min
	// cadence in production) silently kills the sub anchored to oldNC
	// and the wakeup never fires until the CLI's IPC deadline trips.
	wakeup := make(chan struct{}, 1)
	cb := func(msg *nats.Msg) {
		// Subjects are ambiguous between collared and uncollared shapes
		// at certain part counts (see parseSubjectInterpretations).
		// We test every plausible (handle, pipe) split — waking on a
		// false positive is harmless (next snapshot decides), but a
		// missed wakeup would hang the caller.
		for _, iv := range parseSubjectInterpretations(msg.Subject) {
			if matchAnyTarget(iv.Handle, iv.Pipe, req.Patterns) {
				select {
				case wakeup <- struct{}{}:
				default:
				}
				return
			}
		}
	}
	entry, ipcErr := d.armWatch(accountID.String()+".>", cb)
	if ipcErr != nil {
		writeIPCErr(conn, ipcErr)
		return
	}
	defer d.Watches.remove(entry)

	// Initial snapshot.
	reply, e := d.buildFilteredList(ctx, accountID, req.Session, req.Patterns)
	if e != nil {
		writeIPCErr(conn, e)
		return
	}
	if hasUnread(reply) {
		writeIPC(conn, reply)
		return
	}

	// Caught up — block until something matching arrives, or the client
	// gives up. CLI never writes after the initial request, so a Read
	// returning means the socket closed.
	clientGone := make(chan struct{})
	go func() {
		var b [1]byte
		_, _ = conn.Read(b[:])
		close(clientGone)
	}()

	select {
	case <-wakeup:
		reply, e = d.buildFilteredList(ctx, accountID, req.Session, req.Patterns)
		if e != nil {
			writeIPCErr(conn, e)
			return
		}
		writeIPC(conn, reply)
	case <-clientGone:
		return
	case <-ctx.Done():
		return
	}
}

// jetStream returns a JetStream context bound to the daemon's current
// NATS connection, reading d.NC under ncMu so a concurrent swapNC (logout
// via the watchState watcher, or a JWT-rotation reconnect) can't leave the
// caller dereferencing a conn that's been swapped to nil. jetstream.New
// PANICS on a nil conn (setReplyPrefix), and that panic — unrecovered in
// handleConn — took down the whole daemon in the share-inbox-logout flake:
// an in-flight `subs wait` passed ensureNATS, then logout niled d.NC before
// buildFilteredList's unlocked read reached jetstream.New. Every JetStream
// entry point must go through here rather than touching d.NC directly.
// Returns ENATSUnreachable when no connection is currently installed — the
// same fail-soft error every existing call site already mapped a
// jetstream.New failure to.
func (d *Daemon) jetStream() (jetstream.JetStream, *cliproto.Error) {
	d.ncMu.Lock()
	nc := d.NC
	d.ncMu.Unlock()
	if nc == nil {
		return nil, cliproto.New(cliproto.ENATSUnreachable)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, cliproto.New(cliproto.ENATSUnreachable)
	}
	return js, nil
}

// buildFilteredList does the same per-pipe enumeration as handleList,
// but filters by the patterns. Returns a ListReply whose Sources only
// include matching handles AND whose UncollaredPipes only include
// matching uncollared rows. The uncollared listing was previously
// omitted entirely, so any pattern filter on an org with only
// uncollared traffic returned an empty reply and the level-triggered
// early-return at handleListWatch never fired.
func (d *Daemon) buildFilteredList(ctx context.Context, accountID uuid.UUID, session string, patterns []string) (cliproto.ListReply, *cliproto.Error) {
	var lr cliproto.ListSourcesReply
	if e := d.callServer(ctx, "GET", "/api/v1/sources", nil, &lr); e != nil {
		return cliproto.ListReply{}, e
	}
	d.refreshSourceCache(lr.Sources)

	js, e := d.jetStream()
	if e != nil {
		return cliproto.ListReply{}, e
	}

	enriched, err := enrichSourcesWithPipeInfo(ctx, js, lr.Sources, accountID, session, patterns, cursorSnapshot(d.Cursors, session))
	if err != nil {
		return cliproto.ListReply{}, cliproto.New(cliproto.ENATSUnreachable)
	}

	// Mirror handleList's uncollared enrichment, then filter by patterns.
	var ucReply cliproto.ListUncollaredPipesReply
	if e := d.callServer(ctx, "GET", "/api/v1/pipes", nil, &ucReply); e != nil {
		return cliproto.ListReply{}, e
	}
	uncollared := make([]cliproto.UncollaredPipe, 0, len(ucReply.Pipes))
	for _, p := range ucReply.Pipes {
		// Uncollared has handle="". The rendered path is "manifold.name"
		// at namespace or just "name" at root — that string is what
		// matchAnyTarget will check against patterns.
		dotted := cliproto.FormatPipePath(p.Manifold, "", p.Name)
		if !matchAnyTarget("", dotted, patterns) {
			continue
		}
		info := uncollaredPipeInfo(ctx, js, accountID, p.Manifold, p.Name, session, d.Cursors)
		info.CreatedBy = p.CreatedBy
		uncollared = append(uncollared, cliproto.UncollaredPipe{
			Manifold: p.Manifold,
			Name:     p.Name,
			Info:     info,
		})
	}

	return cliproto.ListReply{Sources: enriched, UncollaredPipes: uncollared}, nil
}

// matchAnyTarget returns true if any pattern matches the row's full
// `<handle>.<pipe>` target (for uncollared rows handle is empty, so the
// target is the bare/dotted pipe path). Matching is full-name only — a
// pattern is NOT tried against the handle alone or the pipe segment alone:
//
//   - `room`       matches only the uncollared pipe `room`, NOT a collared
//                  `<handle>.room`. Use `*.room` / `%room` for the latter.
//   - `alice`      matches only an uncollared pipe `alice`, NOT alice's
//                  pipes. Use `alice.*` / `alice%` to watch a whole handle.
//   - `*.stdout`   matches every handle's stdout (glob spans the dot).
//   - `agent-*`    still matches `agent-one.inbox` etc — the `*` spans the
//                  dot, so a handle-prefix glob naturally covers its pipes.
//
// This mirrors shell glob semantics (`ls Mus` vs `ls Mus*`) and keeps the
// matcher predictable. The CLI separately warns when a fully-specified
// literal (no glob chars) matches nothing, steering to the glob form.
//
// Empty patterns slice means "match anything".
//
// Accepts both `*` (standard glob, requires shell-quoting in zsh) and `%`
// (SQL-LIKE-style alias, passes through unquoted). `%` is translated to `*`
// before filepath.Match, so `agent-*` and `agent-%` are interchangeable.
func matchAnyTarget(handle, pipe string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	target := pipe
	if handle != "" {
		target = handle + "." + pipe
	}
	for _, raw := range patterns {
		p := strings.ReplaceAll(raw, "%", "*")
		if ok, _ := filepath.Match(p, target); ok {
			return true
		}
	}
	return false
}

// hasUnread reports whether any pipe in the reply — source-collared
// OR uncollared — has unread > 0. The "should I wake up the watcher?"
// predicate.
func hasUnread(reply cliproto.ListReply) bool {
	for _, s := range reply.Sources {
		for _, p := range s.PipeInfos {
			if p.Unread > 0 {
				return true
			}
		}
	}
	for _, uc := range reply.UncollaredPipes {
		if uc.Info.Unread > 0 {
			return true
		}
	}
	return false
}

// parseSubjectInterpretations returns the plausible (handle, pipe)
// splits for a NATS subject of the form `<acct>.<...rest>`. The wire
// form is ambiguous between collared and uncollared shapes at certain
// part counts — rather than disambiguate (which would need state
// lookup), we return every reasonable interpretation and let
// matchAnyTarget decide. False-positive wakeups are cheap (the next
// snapshot resolves what's actually unread); false negatives would
// hang the caller.
//
// Returned interpretations:
//   2 parts (acct.X):      [{"", "X"}]                                   uncollared at root
//   3 parts (acct.X.Y):    [{"X","Y"}, {"", "X.Y"}]                      collared OR manifold-uncollared
//   4+ parts (acct...Y.Z): [{Y, Z}, {"", parts[1:].join(".")},           collared, root-uncollared-with-dots,
//                           {parts[1:n-1].join("."), Z}]                 manifold-nested-uncollared
func parseSubjectInterpretations(subject string) []struct{ Handle, Pipe string } {
	parts := strings.Split(subject, ".")
	if len(parts) < 2 {
		return nil
	}
	rest := parts[1:] // drop accountID
	switch len(rest) {
	case 1:
		return []struct{ Handle, Pipe string }{{"", rest[0]}}
	case 2:
		return []struct{ Handle, Pipe string }{
			{rest[0], rest[1]},
			{"", strings.Join(rest, ".")},
		}
	default:
		last := rest[len(rest)-1]
		penult := rest[len(rest)-2]
		return []struct{ Handle, Pipe string }{
			{penult, last},
			{"", strings.Join(rest, ".")},
			{strings.Join(rest[:len(rest)-1], "."), last},
		}
	}
}
