package daemon

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

const listPreviewFetchConcurrency = 8

type streamInfoListProvider interface {
	ListStreams(context.Context, ...jetstream.StreamListOpt) jetstream.StreamInfoLister
}

func streamInfoByName(ctx context.Context, js streamInfoListProvider, accountID uuid.UUID, aclEnforced bool) (map[string]*jetstream.StreamInfo, error) {
	lister := js.ListStreams(ctx, jetstream.WithStreamListSubject(natsubj.OrgSubscription(accountID)))
	infos := map[string]*jetstream.StreamInfo{}
	for info := range lister.Info() {
		infos[info.Config.Name] = info
	}
	if err := lister.Err(); err != nil {
		// Under ACL enforcement (Phase 3) stream enumeration is denied:
		// $JS.API.STREAM.LIST carries no stream token, so it cannot be
		// scoped per pipe, and allowing it would expose every pipe's
		// message count regardless of grants.
		//
		// `ppz ls` must still work. Degrade to the server-side view —
		// handles, pipe names and creators, which are org-visible by
		// design (docs/ACL.md: "lists pipes you cannot read, marked;
		// suppresses their contents") — rather than failing the whole
		// verb. Counts and previews are the part that is withheld.
		if aclEnforced && isEnumerationDenied(err) {
			return map[string]*jetstream.StreamInfo{}, nil
		}
		return nil, err
	}
	return infos, nil
}

// isEnumerationDenied distinguishes "the server refused this" from a
// genuine transport failure.
//
// A denied JS API request surfaces either as an explicit permissions
// error or — because the reply never comes — as a timeout on the inbox,
// so both shapes count. A bare timeout is ALSO what an unreachable
// server looks like, which is why callers gate this on the org actually
// enforcing ACLs: without that gate a NATS hiccup would silently render
// `ls` without counts for every org, including ones that never opted in,
// turning a real failure into a quietly degraded page.
func isEnumerationDenied(err error) bool {
	if errors.Is(err, nats.ErrPermissionViolation) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "permission")
}

// perStreamFallbackTimeout bounds each probe in the fallback below.
//
// A refused stream never answers — the reply simply does not come — so
// the probe can only end in a timeout. At the full JS API deadline, and
// run serially, that made `ppz ls` take 15s for a principal with no read
// access: far worse than the missing counts it was added to fix. The
// probes are therefore concurrent AND short, since for this purpose a
// slow answer is worth no more than a refusal.
const perStreamFallbackTimeout = 2 * time.Second

// perStreamInfo asks about each pipe individually, skipping the ones the
// caller is refused. Used when account-wide enumeration is denied.
func perStreamInfo(ctx context.Context, js jetstream.JetStream, accountID uuid.UUID, sources []cliproto.Source) map[string]*jetstream.StreamInfo {
	names := make([]string, 0, len(sources)*4)
	seen := map[string]bool{}
	for _, s := range sources {
		for _, p := range pipesForSource(s) {
			name := natsubj.BuildStreamName(accountID, s.Manifold, s.Handle, p)
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}

	var mu sync.Mutex
	out := map[string]*jetstream.StreamInfo{}
	sem := make(chan struct{}, listPreviewFetchConcurrency)
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pctx, cancel := context.WithTimeout(ctx, perStreamFallbackTimeout)
			defer cancel()
			stream, err := js.Stream(pctx, name)
			if err != nil {
				return // refused, gone, or too slow — leave it unknown
			}
			info, err := stream.Info(pctx)
			if err != nil {
				return
			}
			mu.Lock()
			out[name] = info
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	return out
}

type listPreviewTarget struct {
	streamName string
	seq        uint64
	sourceIdx  int
	infoIdx    int
}

type listPreviewResult struct {
	sourceIdx int
	infoIdx   int
	preview   string
	payload   string
}

// applyRetention copies a stream's configured caps onto the PipeInfo.
//
// This costs nothing extra: the daemon already holds a *StreamInfo per
// pipe (BUFFERED and LAST come from its State), and Config carries the
// caps alongside. So `ls -l` needs no new endpoint and no second round
// trip.
//
// Reading from JetStream rather than the `pipes` table is also the only
// way to answer for auto-provisioned pipes: `inbox` and `stdout` have no
// row until someone runs `pipe set` on them, yet they are exactly the
// pipes whose defaults users hit first. The stream config is what does
// the enforcing, so it is the honest source.
//
// -1 is JetStream's "unlimited" and is carried through rather than
// flattened to 0 — the formatter needs to tell the two apart.
func applyRetention(info *cliproto.PipeInfo, cfg jetstream.StreamConfig) {
	info.TTLSeconds = int(cfg.MaxAge / time.Second)
	info.MaxMsgs = cfg.MaxMsgs
	info.MaxBytes = cfg.MaxBytes
}

func enrichSourcesWithPipeInfo(ctx context.Context, js jetstream.JetStream, sources []cliproto.Source, accountID uuid.UUID, session string, patterns []string, cursors map[string]cursorEntry, long bool, aclEnforced bool) ([]cliproto.Source, error) {
	// Under enforcement, account-wide enumeration is ALWAYS denied —
	// $JS.API.STREAM.LIST carries no stream token, so it cannot be
	// scoped per pipe and is refused for every principal including the
	// org owner. Attempting it anyway costs a full JS API timeout before
	// the reply that never comes gives up, which made `ppz ls` take six
	// seconds for everyone. Go straight to the per-stream path.
	//
	// That path matters for correctness too: with no stream info every
	// collared pipe renders as 0/0, which reads as "empty" rather than
	// "unknown" — `ppz ls` under-reporting a pipe that has messages, to
	// its own owner. Per-stream INFO *is* grantable, so the caller gets
	// real counts for what it may read and nothing for the rest, which
	// is the honest answer.
	var streamInfos map[string]*jetstream.StreamInfo
	if aclEnforced {
		streamInfos = perStreamInfo(ctx, js, accountID, sources)
	} else {
		var err error
		streamInfos, err = streamInfoByName(ctx, js, accountID, aclEnforced)
		if err != nil {
			return nil, err
		}
	}

	enriched := make([]cliproto.Source, 0, len(sources))
	previewTargets := make([]listPreviewTarget, 0)
	for _, s := range sources {
		pipes := pipesForSource(s)
		// The server populates PipeInfos for user-created pipes with
		// their CreatedBy username. Auto-pipes (broadcast / inbox /
		// stdin / stdout / stdctrl) aren't in the `pipes` table so
		// they have no row here — the formatter inherits Source.CreatedBy
		// at render time. Capture the map so we can carry the
		// per-pipe creator through the JetStream enrichment that
		// otherwise rebuilds PipeInfo from scratch.
		pipeCreator := make(map[string]string, len(s.PipeInfos))
		for _, pi := range s.PipeInfos {
			if pi.CreatedBy != "" {
				pipeCreator[pi.Pipe] = pi.CreatedBy
			}
		}
		infos := make([]cliproto.PipeInfo, 0, len(pipes))
		for _, p := range pipes {
			if !matchAnyTarget(s.Handle, p, patterns) {
				continue
			}

			info := cliproto.PipeInfo{Pipe: p, CreatedBy: pipeCreator[p]}
			streamName := natsubj.BuildStreamName(accountID, s.Manifold, s.Handle, p)
			if si := streamInfos[streamName]; si != nil {
				if long {
					applyRetention(&info, si.Config)
				}
				info.Total = si.State.Msgs
				info.LastSeq = si.State.LastSeq
				if !si.State.LastTime.IsZero() {
					lt := si.State.LastTime.UTC()
					info.LastAt = &lt
				}
				// effectiveCursor resets a cursor stamped against a prior
				// incarnation of this stream (source recreated) so a fresh
				// stream's messages all count as unread instead of being
				// hidden behind the stale watermark.
				cursor := effectiveCursor(cursors[daemonCursorKey(accountID, s.Handle, p)], createdNanos(si.Created), si.State.LastSeq)
				if info.LastSeq > cursor {
					// Cap at Total (buffered count): messages whose
					// seq is below the stream's FirstSeq have been
					// purged by TTL / msg-cap and can never be read,
					// so reporting them as unread strands the user.
					info.Unread = min(info.LastSeq-cursor, info.Total)
				}
				if info.LastSeq > 0 {
					previewTargets = append(previewTargets, listPreviewTarget{
						streamName: streamName,
						seq:        info.LastSeq,
						sourceIdx:  len(enriched),
						infoIdx:    len(infos),
					})
				}
			}
			infos = append(infos, info)
		}
		if len(patterns) > 0 && len(infos) == 0 {
			continue
		}
		s.PipeInfos = infos
		enriched = append(enriched, s)
	}

	for result := range fetchListPreviews(ctx, js, previewTargets) {
		enriched[result.sourceIdx].PipeInfos[result.infoIdx].Preview = result.preview
		enriched[result.sourceIdx].PipeInfos[result.infoIdx].Payload = result.payload
	}

	return enriched, nil
}

func pipesForSource(s cliproto.Source) []string {
	pipeSet := map[string]struct{}{}
	for _, p := range pipesForKind(s.Kind) {
		pipeSet[p] = struct{}{}
	}
	for _, p := range s.Pipes {
		pipeSet[p] = struct{}{}
	}
	pipes := make([]string, 0, len(pipeSet))
	for p := range pipeSet {
		pipes = append(pipes, p)
	}
	sort.Strings(pipes)
	return pipes
}

func fetchListPreviews(ctx context.Context, js jetstream.JetStream, targets []listPreviewTarget) <-chan listPreviewResult {
	results := make(chan listPreviewResult)
	if len(targets) == 0 {
		close(results)
		return results
	}

	jobs := make(chan listPreviewTarget)
	workerCount := listPreviewFetchConcurrency
	if len(targets) < workerCount {
		workerCount = len(targets)
	}

	var wg sync.WaitGroup
	wg.Add(workerCount)
	for range workerCount {
		go func() {
			defer wg.Done()
			for target := range jobs {
				stream, err := js.Stream(ctx, target.streamName)
				if err != nil {
					continue
				}
				msg, err := stream.GetMsg(ctx, target.seq)
				if err != nil {
					continue
				}
				env, err := envelope.Unmarshal(msg.Data)
				if err != nil {
					continue
				}
				select {
				case results <- listPreviewResult{
					sourceIdx: target.sourceIdx,
					infoIdx:   target.infoIdx,
					preview:   cliproto.TruncatePayload(env.Payload),
					payload:   env.Payload,
				}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, target := range targets {
			select {
			case jobs <- target:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	return results
}

func cursorSnapshot(c *cursors, session string) map[string]cursorEntry {
	session = sessionIDForCursor(session)
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadLocked(session)
	if err != nil {
		return map[string]cursorEntry{}
	}
	return m
}

func sessionIDForCursor(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
