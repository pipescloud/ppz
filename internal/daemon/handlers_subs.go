package daemon

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// handleSubsAdd appends targets to the caller's session subscription list.
// Idempotent (see subscriptions.Add). Targets are stored verbatim — a
// dotless target stays an uncollared pipe (read-style); the daemon does no
// `.inbox` sugaring.
func (d *Daemon) handleSubsAdd(_ context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.SubsAddRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	if err := d.Subs.Add(req.Session, req.Targets...); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	writeIPC(conn, cliproto.SubsAddReply{Subs: d.Subs.List(req.Session)})
}

// handleSubsRemove drops targets from the session list. Removing an agent's
// OWN inbox — the auto-subscribed <session>.inbox — is guarded: refused
// (E_OWN_INBOX) unless Force, so an agent doesn't accidentally opt out of
// its own monitor. Removing any other subject (including a DIFFERENT
// handle's inbox) is allowed and idempotent.
func (d *Daemon) handleSubsRemove(_ context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.SubsRemoveRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	// "self" is defined by the session key: inside `ppz terminal share H`,
	// PPZ_SESSION=H, so H.inbox is the agent's own inbox. From a personal
	// shell (session sid-N), no target is "self".
	ownInbox := session(req.Session) + ".inbox"
	if !req.Force {
		for _, t := range req.Targets {
			if strings.TrimSpace(t) == ownInbox {
				writeIPCErr(conn, &cliproto.Error{
					Code:    "E_OWN_INBOX",
					Message: "refusing to remove own inbox " + ownInbox + " (use --force to override)",
				})
				return
			}
		}
	}
	// Snapshot the stored set BEFORE removal so we know which targets were
	// actually stored subjects (Removed). Removal itself is unchanged:
	// exact-string-match, idempotent.
	before := d.Subs.List(req.Session)
	inStore := make(map[string]bool, len(before))
	for _, s := range before {
		inStore[s] = true
	}
	if err := d.Subs.Remove(req.Session, req.Targets...); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()})
		return
	}
	// Coverage is judged against the patterns STILL present after removal, so
	// (a) a removed literal that re-expands under a surviving pattern gets a
	// "still matched by" hint rather than a falsely reassuring bare "removed",
	// and (b) a pattern removed in this same call no longer claims to cover
	// anything.
	after := d.Subs.List(req.Session)
	var remainingPatterns []string
	for _, s := range after {
		if cliproto.IsGlobPattern(s) {
			remainingPatterns = append(remainingPatterns, s)
		}
	}
	var outcomes []cliproto.SubsRemoveOutcome
	for _, raw := range req.Targets {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		outcomes = append(outcomes, cliproto.SubsRemoveOutcome{
			Target:           t,
			Removed:          inStore[t],
			CoveredByPattern: firstCoveringPattern(t, remainingPatterns),
		})
	}
	writeIPC(conn, cliproto.SubsRemoveReply{Subs: after, Outcomes: outcomes})
}

// firstCoveringPattern returns the first pattern (in the order given, which
// is sorted) that matches target via the same %→* / filepath.Match semantics
// as matchAnyTarget. Empty string if none — used by `subs rm` feedback to
// explain that a target is (still) surfaced by a pattern: either it was never
// its own sub, or its literal sub was removed but the pattern re-expands it.
func firstCoveringPattern(target string, patterns []string) string {
	for _, p := range patterns {
		g := strings.ReplaceAll(p, "%", "*")
		if ok, _ := filepath.Match(g, target); ok {
			return p
		}
	}
	return ""
}

// handleSubsList replies with a ListReply scoped to the session's
// subscription set (the source of truth), enriched with live JetStream
// stats where the pipe exists and synthetic zero-rows where it doesn't.
func (d *Daemon) handleSubsList(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.SubsListRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	reply, e := d.subsSnapshot(ctx, req.Session)
	if e != nil {
		writeIPCErr(conn, e)
		return
	}
	writeIPC(conn, reply)
}

// handleSubsWait is `ppz subs wait` — `ls --watch` scoped to the
// subscription set. Level-triggered: returns immediately if any subscribed
// pipe already has unread; otherwise subscribes to the org subject space
// and blocks until a publish on a subscribed subject lands.
//
// The reply carries ONLY the unread row(s) — token-light for an agent
// monitor loop; the full set is `ppz subs ls`.
func (d *Daemon) handleSubsWait(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.SubsWaitRequest
	if err := json.Unmarshal(params, &req); err != nil {
		writeIPCErr(conn, &cliproto.Error{Code: "E_PROTOCOL", Message: err.Error()})
		return
	}
	if _, ok := d.State.Credentials(); !ok {
		writeIPCErr(conn, cliproto.New(cliproto.ENotLoggedIn))
		return
	}
	subjects := d.Subs.List(req.Session)
	// No subscriptions → nothing can ever wake us; return an empty snapshot
	// immediately rather than block forever.
	if len(subjects) == 0 {
		writeIPC(conn, cliproto.ListReply{})
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

	// Subscribe BEFORE the initial snapshot (same ordering rationale as
	// handleListWatch) so an arrival between snapshot and subscribe can't
	// strand the caller. Wake on any publish matching a subscribed subject;
	// false positives are harmless — the next snapshot decides.
	//
	// armWatch registers the sub with d.Watches so a swapNCLocked rearms
	// it onto the new conn — without that, a JWT-refresh swap (~10min
	// cadence in production) silently kills the sub anchored to oldNC
	// and the wakeup never fires until the CLI's IPC deadline trips.
	wakeup := make(chan struct{}, 1)
	cb := func(msg *nats.Msg) {
		for _, iv := range parseSubjectInterpretations(msg.Subject) {
			if matchAnyTarget(iv.Handle, iv.Pipe, subjects) {
				select {
				case wakeup <- struct{}{}:
				default:
				}
				return
			}
		}
	}
	entry, e := d.armWatch(accountID.String()+".>", cb)
	if e != nil {
		writeIPCErr(conn, e)
		return
	}
	defer d.Watches.remove(entry)

	reply, e := d.subsSnapshot(ctx, req.Session)
	if e != nil {
		writeIPCErr(conn, e)
		return
	}
	if unread := unreadOnly(reply); hasUnread(unread) {
		writeIPC(conn, unread)
		return
	}

	clientGone := make(chan struct{})
	go func() {
		var b [1]byte
		_, _ = conn.Read(b[:])
		close(clientGone)
	}()

	select {
	case <-wakeup:
		reply, e = d.subsSnapshot(ctx, req.Session)
		if e != nil {
			writeIPCErr(conn, e)
			return
		}
		writeIPC(conn, unreadOnly(reply))
	case <-clientGone:
		return
	case <-ctx.Done():
		return
	}
}

// subsSnapshot builds a ListReply for the session's subscription set. It
// reuses buildFilteredList to enrich subscribed pipes that currently exist
// (correct unread / buffered / creator), then appends synthetic zero-rows
// for subscribed subjects with no backing pipe yet (e.g. a room nobody has
// posted to). The subscription list is the source of truth: every
// subscribed subject appears as a row.
func (d *Daemon) subsSnapshot(ctx context.Context, sessionID string) (cliproto.ListReply, *cliproto.Error) {
	subjects := d.Subs.List(sessionID)
	if len(subjects) == 0 {
		return cliproto.ListReply{}, nil
	}
	if _, ok := d.State.Credentials(); !ok {
		return cliproto.ListReply{}, cliproto.New(cliproto.ENotLoggedIn)
	}
	accountID, err := uuid.Parse(d.State.AccountID())
	if err != nil {
		return cliproto.ListReply{}, &cliproto.Error{Code: "E_INTERNAL", Message: "bad org id"}
	}
	if err := d.ensureNATS(ctx); err != nil {
		if e, ok := err.(*cliproto.Error); ok {
			return cliproto.ListReply{}, e
		}
		return cliproto.ListReply{}, &cliproto.Error{Code: "E_INTERNAL", Message: err.Error()}
	}

	// Subjects are matched by buildFilteredList → matchAnyTarget on the full
	// <handle>.<pipe> target, so a bare uncollared sub like `room` matches
	// only the uncollared `room` pipe — not a collared `<handle>.room`. (To
	// follow a collared pipe, subscribe to the explicit `<handle>.<pipe>` or
	// a glob like `*.room`.)
	// long=false: `subs ls` answers "what am I subscribed to", not "how
	// is this pipe configured".
	reply, e := d.buildFilteredList(ctx, accountID, sessionID, subjects, false)
	if e != nil {
		return cliproto.ListReply{}, e
	}

	// Which subjects did buildFilteredList already surface (existing pipes)?
	covered := map[string]bool{}
	for _, s := range reply.Sources {
		for _, p := range s.PipeInfos {
			covered[s.Handle+"."+p.Pipe] = true
		}
	}
	for _, u := range reply.UncollaredPipes {
		covered[cliproto.FormatPipePath(u.Manifold, "", u.Name)] = true
	}

	// Append synthetic zero-rows for subscribed-but-nonexistent subjects so
	// `subs ls` shows the full subscription list. Dotted → collared (grouped
	// into the handle's Source row); dotless → uncollared room/lobby.
	//
	// Glob/pattern subjects are skipped: they have no literal pipe of their
	// own — they expand at read-time (via buildFilteredList's matchAnyTarget,
	// above) to whatever currently matches, so a pattern surfaces as its
	// matches or as nothing, never as a spurious literal row.
	for _, subj := range subjects {
		if covered[subj] || isGlobPattern(subj) {
			continue
		}
		if handle, pipe, ok := splitCollared(subj); ok {
			reply.Sources = appendSyntheticPipe(reply.Sources, handle, pipe)
		} else {
			reply.UncollaredPipes = append(reply.UncollaredPipes, cliproto.UncollaredPipe{
				Name: subj,
				Info: cliproto.PipeInfo{Pipe: subj},
			})
		}
	}
	sortListReply(&reply)

	// Attribution: tag each surfaced pipe with the subscribed subject(s) that
	// matched it, and carry the verbatim subscription list. The CLI uses these
	// to render pattern subs as parent rows (incl. a pattern that matches
	// nothing, which has no pipe row) and to emit matched_by in --json.
	for i := range reply.Sources {
		h := reply.Sources[i].Handle
		for j := range reply.Sources[i].PipeInfos {
			reply.Sources[i].PipeInfos[j].MatchedBy = matchedSubjects(h, reply.Sources[i].PipeInfos[j].Pipe, subjects)
		}
	}
	for i := range reply.UncollaredPipes {
		reply.UncollaredPipes[i].Info.MatchedBy = matchedSubjects("", reply.UncollaredPipes[i].Name, subjects)
	}
	reply.Subscriptions = subjects
	return reply, nil
}

// matchedSubjects returns the subscribed subjects matching (handle, pipe),
// sorted — the same %→* / full-`<handle>.<pipe>` semantics matchAnyTarget
// uses for filtering, run per-subject to record WHICH ones matched.
func matchedSubjects(handle, pipe string, subjects []string) []string {
	var out []string
	for _, s := range subjects {
		if matchAnyTarget(handle, pipe, []string{s}) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// unreadOnly returns a copy of reply keeping only rows with unread > 0 —
// the token-light payload `subs wait` emits on wake.
func unreadOnly(in cliproto.ListReply) cliproto.ListReply {
	var out cliproto.ListReply
	for _, s := range in.Sources {
		kept := make([]cliproto.PipeInfo, 0, len(s.PipeInfos))
		for _, p := range s.PipeInfos {
			if p.Unread > 0 {
				kept = append(kept, p)
			}
		}
		if len(kept) > 0 {
			s.PipeInfos = kept
			out.Sources = append(out.Sources, s)
		}
	}
	for _, u := range in.UncollaredPipes {
		if u.Info.Unread > 0 {
			out.UncollaredPipes = append(out.UncollaredPipes, u)
		}
	}
	return out
}

// isGlobPattern reports whether a subscribed subject is a glob/pattern
// rather than a concrete subject. Pattern subs expand at read-time, so they
// never get a synthetic literal row in subsSnapshot. Delegates to the shared
// cliproto definition so the daemon and the CLI tree renderer agree.
func isGlobPattern(s string) bool {
	return cliproto.IsGlobPattern(s)
}

// splitCollared mirrors `ppz read` target parsing: a dotted target is
// collared (`<handle>.<pipe>`, split on the last dot); a dotless target is
// an uncollared pipe and returns ok=false.
func splitCollared(subject string) (handle, pipe string, ok bool) {
	idx := strings.LastIndex(subject, ".")
	if idx <= 0 || idx == len(subject)-1 {
		return "", "", false
	}
	return subject[:idx], subject[idx+1:], true
}

// appendSyntheticPipe adds a zero-stat PipeInfo for (handle, pipe), grouping
// it into an existing Source row for that handle when present.
func appendSyntheticPipe(sources []cliproto.Source, handle, pipe string) []cliproto.Source {
	for i := range sources {
		if sources[i].Handle == handle {
			sources[i].PipeInfos = append(sources[i].PipeInfos, cliproto.PipeInfo{Pipe: pipe})
			return sources
		}
	}
	return append(sources, cliproto.Source{
		Handle:    handle,
		PipeInfos: []cliproto.PipeInfo{{Pipe: pipe}},
	})
}

// sortListReply orders the reply deterministically: Sources by handle (each
// source's pipes by name), uncollared by rendered path.
func sortListReply(r *cliproto.ListReply) {
	sort.Slice(r.Sources, func(i, j int) bool { return r.Sources[i].Handle < r.Sources[j].Handle })
	for i := range r.Sources {
		pis := r.Sources[i].PipeInfos
		sort.Slice(pis, func(a, b int) bool { return pis[a].Pipe < pis[b].Pipe })
	}
	sort.Slice(r.UncollaredPipes, func(i, j int) bool {
		return cliproto.FormatPipePath(r.UncollaredPipes[i].Manifold, "", r.UncollaredPipes[i].Name) <
			cliproto.FormatPipePath(r.UncollaredPipes[j].Manifold, "", r.UncollaredPipes[j].Name)
	})
}
