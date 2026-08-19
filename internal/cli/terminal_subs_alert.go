package cli

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
)

const terminalSubsAlertMessage = "Please run 'ppz subs read' and action messages\n"

// Production alert cadence. The nag is a backstop for an agent that
// has stopped watching its own subs, not a metronome: agents poll
// their pipes themselves, so the common case should resolve before
// the pump ever injects.
//
//   - IdleAfter 60s: wide enough that the agent's own watch usually
//     reads the message first — the fire-time ConfirmUnread then
//     suppresses the nudge and nothing reaches the PTY at all —
//     while still prompt enough that a genuinely parked agent isn't
//     sitting on an unread message for minutes.
//   - Cooldown 5m / CooldownMax 30m: the repeat ladder. See
//     CooldownMax on terminalSubsAlertConfig for why repeats escalate.
const (
	defaultSubsAlertIdleAfter   = 60 * time.Second
	defaultSubsAlertCooldown    = 5 * time.Minute
	defaultSubsAlertCooldownMax = 30 * time.Minute
)

type terminalSubsAlertConfig struct {
	IdleAfter time.Duration
	// Cooldown is the BASE gap between repeat alerts — the gap after
	// the first unacknowledged alert. Subsequent gaps double (see
	// CooldownMax).
	Cooldown time.Duration
	// CooldownMax is the ceiling on the repeat-alert backoff ladder.
	//
	// A repeat alert is, by definition, evidence that the previous one
	// did not work: the agent was told and the message is still
	// unread. So repeats escalate — the gap after the Nth consecutive
	// unacknowledged alert is min(Cooldown * 2^(N-1), CooldownMax).
	//
	// Without the ladder a flat Cooldown nags forever at full rate. The
	// reported failure is a session usage limit: the agent hits its
	// limit with a message unread, cannot run `ppz subs read` for n
	// hours, and every injected nag is queued as a real turn that
	// flushes the moment the limit lifts — ~600 of them across a
	// 5-hour block at a 30s cadence, burning context window and token
	// budget. The ladder bounds the same block to ~12.
	//
	// The rung counts alerts INJECTED since the last proof the agent
	// consumed anything, and the only such proof is a negative
	// ConfirmUnread — the unread level actually dropped. Message
	// ARRIVALS are not proof and must not reset the ladder, or a busy
	// pipe reproduces the flood in full. Suppressed and deferred fires
	// inject nothing and so consume no rung.
	//
	// Known limitation: a read that lands DURING a backoff window is
	// not observed until that window opens, because ConfirmUnread only
	// runs at fire time — the in-flight window itself is never
	// shortened. What IS guaranteed (via the consumption watermark,
	// see terminalSubsAlertStateMachine.baseline) is that the read is
	// credited when the window opens, even if new traffic has kept the
	// unread level high the whole time: the fire resets the ladder and
	// subsequent gaps return to base. A responsive agent therefore
	// never climbs, and the one inherited window is bounded by
	// CooldownMax.
	CooldownMax time.Duration
	Message     string
	// Harness identifies which agent harness the wrapped PTY is
	// running (one of "claude" / "copilot" / "codex" / "agy" /
	// "pi", or empty for non-agent shares). Used by
	// submitAlertToPTY to pick the right submit-key byte
	// sequence — claude reads `\x1b[13u` (kitty keyboard protocol
	// Enter), other harnesses' REPLs treat that escape as literal
	// bytes and need a plain `\r` to submit.
	Harness string
	// ConfirmUnread is consulted at fire time — after the pending /
	// idle / cooldown gates have all passed, immediately before the
	// alert is injected. It must re-sample the live subs snapshot
	// (production wires `ppz subs ls` over IPC) and return it raw;
	// the state machine derives the fire/suppress decision via
	// confirmSubsUnreadDecision. Nothing unread suppresses the
	// injection AND clears pending: the level is the source of
	// truth, and a pending bit that disagrees with it is stale by
	// definition. An IPC error maps to fire-anyway (see
	// confirmSubsUnreadDecision). nil → always fire (tests of
	// unrelated behaviour skip the wiring).
	//
	// This gate exists because `subs wait` only signals the up-edge
	// (something became unread). The down-edge — the agent ran
	// `ppz subs read`, cursor advanced, nothing published — produces
	// no wakeup, so a pending bit armed up to 250ms before the read
	// survives it and would otherwise fire one final redundant nag.
	ConfirmUnread func() (cliproto.ListReply, error)
}

type terminalSubsAlertStateMachine struct {
	mu            sync.Mutex
	cfg           terminalSubsAlertConfig
	pending       bool
	pendingSince  time.Time
	lastUserInput time.Time
	lastAlert     time.Time
	// unacked counts alerts INJECTED since the last proof the agent
	// consumed anything. It drives the repeat-backoff ladder (see
	// CooldownMax). Two observations reset it: a negative
	// ConfirmUnread (the level dropped to zero), or a consumption
	// watermark advance at fire time (see baseline). Suppressed and
	// deferred fires inject nothing and so never increment it.
	unacked int
	// baseline is the per-pipe consumption watermark (Total - Unread,
	// clamped at zero) snapshotted from the confirm reply at the
	// moment of the LAST INJECTION; nil until the first successful
	// one. A later fire whose confirm shows any shared pipe's
	// watermark above this baseline proves the agent consumed since
	// the last alert — even when the level never touched zero because
	// new traffic kept arriving (the field-observed occluded-read
	// case) — and resets unacked before the fire counts itself.
	//
	// The baseline may ONLY move at injection time, and only from a
	// successful confirm: a deferral's reply must not re-baseline (it
	// would destroy the advance it carries before anything fired),
	// and an error-fire retains the last good baseline so a read
	// around a daemon hiccup is still credited at the next successful
	// confirm. Pipes are compared only when present in both snapshots
	// — a freshly subscribed pipe's pre-existing history proves
	// nothing about consumption since the last alert.
	//
	// Deliberate looseness: ANY pipe advancing resets the whole
	// ladder, so an agent reading its inbox while ignoring one
	// subscribed room holds the base cadence for that room
	// indefinitely. That follows from the rule the ladder encodes —
	// back off an agent that ISN'T consuming, never one that is — a
	// selectively-inattentive agent is reachable at base cadence by
	// definition, so per-pipe ladders would add state without adding
	// reach.
	baseline map[string]uint64
}

func newTerminalSubsAlertStateMachine(cfg terminalSubsAlertConfig) *terminalSubsAlertStateMachine {
	if cfg.IdleAfter <= 0 {
		cfg.IdleAfter = defaultSubsAlertIdleAfter
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = defaultSubsAlertCooldown
	}
	if cfg.CooldownMax <= 0 {
		cfg.CooldownMax = defaultSubsAlertCooldownMax
	}
	if cfg.Message == "" {
		cfg.Message = terminalSubsAlertMessage
	}
	return &terminalSubsAlertStateMachine{cfg: cfg}
}

func (s *terminalSubsAlertStateMachine) ObserveUserInput(now time.Time, input []byte) {
	if len(input) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUserInput = now
}

// ObserveSubsUnread flips the pending bit (idempotent on repeat
// observations within a single pending window). The first call
// after a clear stamps pendingSince so the idle-after gate can
// measure how long the unread has been outstanding without user
// input.
func (s *terminalSubsAlertStateMachine) ObserveSubsUnread(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pending {
		s.pendingSince = now
	}
	s.pending = true
}

// ObserveSubsUnreadSnapshot is ObserveSubsUnread plus the row state
// the subs-wait wakeup already carries. Before the first injection
// the ladder has no baseline, so the arm-time watermarks seed it —
// that is what lets a fire attempt detect "the agent consumed during
// this pending window" for an episode that has never alerted (the
// stale-idle-window bug). Seeding only when nil is deliberate:
// post-injection the baseline must stay pinned to the last injection,
// or reads between injections would be erased by the next re-arm.
func (s *terminalSubsAlertStateMachine) ObserveSubsUnreadSnapshot(now time.Time, reply cliproto.ListReply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pending {
		s.pendingSince = now
	}
	s.pending = true
	if s.baseline == nil {
		s.baseline = subsReplyWatermarks(reply)
	}
}

func (s *terminalSubsAlertStateMachine) ReadyAlert(now time.Time) string {
	s.mu.Lock()
	if !s.ready(now) {
		s.mu.Unlock()
		return ""
	}
	confirm := s.cfg.ConfirmUnread
	s.mu.Unlock()

	// Fire-time confirmation runs OUTSIDE the lock: it's an IPC round
	// trip in production, and ObserveUserInput (called from the stdin
	// forwarding path) takes the same mutex — holding it here would
	// stall the user's keystrokes for the duration of the confirm.
	var wm map[string]uint64
	var newestUnread time.Time
	if confirm != nil {
		reply, err := confirm()
		if err == nil {
			wm = subsReplyWatermarks(reply)
			newestUnread = subsReplyNewestUnreadAt(reply)
		}
		if !confirmSubsUnreadDecision(reply, err) {
			// The level says nothing is unread; the pending bit is stale
			// by definition — clear it so the next tick doesn't
			// re-confirm forever. lastAlert is deliberately NOT stamped:
			// nothing was injected, so a suppressed fire must not push
			// the cooldown out and delay the next real alert. If a new
			// message arrived during the confirm, the subs-wait loop's
			// level-triggered re-arm (≤250ms) re-raises pending.
			s.mu.Lock()
			s.pending = false
			// The level dropped: the agent read its messages. That is
			// the only evidence the nag is landing, so it returns the
			// repeat cadence to the base gap. Without the reset a single
			// stalled stretch would leave the pump permanently
			// desensitised for the rest of the share session.
			s.unacked = 0
			s.mu.Unlock()
			return ""
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-check: user input may have arrived while the confirm was in
	// flight, closing the idle gate (lastUserInput moved past `now`).
	if !s.ready(now) {
		return ""
	}
	// Consumed-episode deferral: the agent read something during this
	// pending window (watermark advance — the same consumption proof
	// the ladder keys on) and the surviving unread is YOUNG. Injecting
	// now would nag for a message that has had only seconds of the
	// idle grace the gate exists to provide (field-observed: 5s), so
	// suppress and restamp — the survivor earns a full window of its
	// own. Adopting the confirm's watermarks as the new baseline means
	// one read buys exactly one deferral: further arrivals without
	// further reads fire at the fresh gate regardless of youth (no
	// starvation), as does an OLD survivor (the post-cooldown shape —
	// deferring a message that already waited minutes adds pure
	// latency), or a snapshot with no LastAt to prove youth. The
	// advance also resets the ladder, and pending stays true: there IS
	// unread, the question was only when to say so.
	if wm != nil && watermarkAdvanced(s.baseline, wm) &&
		!newestUnread.IsZero() && now.Sub(newestUnread) < s.cfg.IdleAfter {
		s.pendingSince = now
		s.baseline = wm
		s.unacked = 0
		return ""
	}

	s.pending = false
	s.lastAlert = now
	if wm != nil {
		if watermarkAdvanced(s.baseline, wm) {
			// The agent consumed something since the last alert. The
			// level-only view missed it — new traffic kept the confirm
			// reporting "unread" — but consumption is consumption: this
			// fire is a fresh episode's first alert, not a repeat that
			// failed.
			s.unacked = 0
		}
		s.baseline = wm
	}
	s.unacked++
	return s.cfg.Message
}

// subsReplyWatermarks extracts the per-pipe consumption watermark
// (Total - Unread) from a fire-time snapshot, walking both row
// families the same way subsReplyHasUnread does. Keys are prefixed by
// family so a source pipe and an uncollared pipe sharing a name can't
// collide. Unread > Total (a mid-update daemon race or bug) clamps to
// zero — on uint64 the naive subtraction underflows to ~2^64 and
// would read as an enormous advance.
func subsReplyWatermarks(reply cliproto.ListReply) map[string]uint64 {
	wm := make(map[string]uint64)
	clamped := func(info cliproto.PipeInfo) uint64 {
		if info.Unread > info.Total {
			return 0
		}
		return info.Total - info.Unread
	}
	for _, src := range reply.Sources {
		for _, p := range src.PipeInfos {
			// Manifold is part of the key: handle uniqueness is per
			// (account, manifold) — 0002_manifold.sql — so "zif" at the
			// root and "zif" in team1 can share one snapshot, and a
			// manifold-less key would let one shadow the other's reads.
			wm["s/"+src.Manifold+"/"+src.Handle+"/"+p.Pipe] = clamped(p)
		}
	}
	for _, u := range reply.UncollaredPipes {
		wm["u/"+u.Manifold+"/"+u.Name] = clamped(u.Info)
	}
	return wm
}

// subsReplyNewestUnreadAt returns the newest LastAt across rows that
// carry unread, walking both row families — the youngness input for
// the consumed-episode deferral. Zero when no unread row is stamped:
// youth is then unprovable and the fire proceeds (at-least-once — a
// daemon that stops stamping LastAt must not silently widen every
// alert by an idle window). Note LastAt is the pipe's last MESSAGE
// time, which for a single fresh unread is its arrival; with several
// unread it overstates the oldest one's youth, deferring at most one
// extra window and only while reads keep happening.
func subsReplyNewestUnreadAt(reply cliproto.ListReply) time.Time {
	var newest time.Time
	consider := func(info cliproto.PipeInfo) {
		if info.Unread > 0 && info.LastAt != nil && info.LastAt.After(newest) {
			newest = *info.LastAt
		}
	}
	for _, src := range reply.Sources {
		for _, p := range src.PipeInfos {
			consider(p)
		}
	}
	for _, u := range reply.UncollaredPipes {
		consider(u.Info)
	}
	return newest
}

// watermarkAdvanced reports whether any pipe present in BOTH
// snapshots consumed forward. Strictly greater: an unchanged
// watermark proves nothing, and a DECREASE (retention expiry of
// already-read messages shrinks Total without touching Unread) is not
// consumption either. Pipes only in `next` are freshly subscribed —
// their pre-existing history is excluded by the both-snapshots rule.
func watermarkAdvanced(baseline, next map[string]uint64) bool {
	for key, w := range next {
		if prev, ok := baseline[key]; ok && w > prev {
			return true
		}
	}
	return false
}

func (s *terminalSubsAlertStateMachine) ready(now time.Time) bool {
	if !s.pending {
		return false
	}
	if !s.lastUserInput.IsZero() && now.Sub(s.lastUserInput) < s.cfg.IdleAfter {
		return false
	}
	if s.lastUserInput.IsZero() && !s.pendingSince.IsZero() && now.Sub(s.pendingSince) < s.cfg.IdleAfter {
		return false
	}
	if gap := s.repeatCooldown(); gap > 0 && !s.lastAlert.IsZero() && now.Sub(s.lastAlert) < gap {
		return false
	}
	return true
}

// repeatCooldown returns the gap required before the next alert:
// min(Cooldown * 2^(unacked-1), CooldownMax). Callers hold s.mu.
//
// The doubling is what bounds a nag that isn't working. unacked is the
// number of alerts already injected with no proof the agent consumed
// anything, so each repeat is evidence the previous one failed to land
// and buys a longer silence. See CooldownMax for the failure this
// prevents.
//
// A CooldownMax below Cooldown is a misconfiguration, not an
// instruction to nag faster than the operator asked: the base gap is a
// floor, so the ceiling is raised to meet it rather than shrinking the
// first repeat. The loop exits at the ceiling rather than computing
// the full shift, so a long block can't overflow the doubling.
func (s *terminalSubsAlertStateMachine) repeatCooldown() time.Duration {
	base := s.cfg.Cooldown
	if base <= 0 || s.unacked <= 1 {
		return base
	}
	ceiling := s.cfg.CooldownMax
	if ceiling < base {
		ceiling = base
	}
	gap := base
	for i := 1; i < s.unacked; i++ {
		if gap >= ceiling {
			return ceiling
		}
		gap *= 2
	}
	if gap > ceiling {
		gap = ceiling
	}
	return gap
}

// confirmSubsUnreadDecision maps a fire-time subs snapshot (reply, err)
// to "should the pump inject". The production ConfirmUnread wiring
// calls IPCSubsList and feeds the outcome through here.
//
// Only a POSITIVE "nothing unread" (err == nil, no unread rows)
// suppresses the alert; an IPC failure maps to fire-anyway — the nag
// is at-least-once, and a daemon hiccup at fire time must never
// silently eat it. A redundant alert for a just-read message is
// annoying; a swallowed alert for an unread one loses a message.
func confirmSubsUnreadDecision(reply cliproto.ListReply, err error) bool {
	if err != nil {
		return true
	}
	return subsReplyHasUnread(reply)
}

type terminalSubsAlertPump struct {
	mu     sync.Mutex
	sm     *terminalSubsAlertStateMachine
	pty    io.Writer
	write  func(string)
	alert  bool
	buffer []byte
}

func newTerminalSubsAlertPump(cfg terminalSubsAlertConfig, pty io.Writer) *terminalSubsAlertPump {
	if cfg.Message == "" {
		cfg.Message = terminalSubsAlertMessage
	}
	harness := cfg.Harness
	return &terminalSubsAlertPump{
		sm:  newTerminalSubsAlertStateMachine(cfg),
		pty: pty,
		write: func(message string) {
			_ = submitAlertToPTY(pty, harness, message, time.Sleep)
		},
	}
}

// newTerminalSubsAlertPumpForPTY builds the production pump for a real
// PTY master. The master file is only used for echo suppression around
// alert injection; every byte the pump injects — alert submissions and
// buffered user-input flushes alike — goes through w instead, so the
// wrapper can interpose harnessInputWriter and injected bytes taint
// detection causality exactly like local keystrokes. Writing to the
// raw master would let the alert's echo read as untainted agent output
// and flash `ppz who` to working (docs/specs/agent-detection.md).
func newTerminalSubsAlertPumpForPTY(cfg terminalSubsAlertConfig, pty *os.File, w io.Writer) *terminalSubsAlertPump {
	pump := newTerminalSubsAlertPump(cfg, w)
	harness := cfg.Harness
	pump.write = func(message string) {
		restore := setPTYInputEcho(pty.Fd(), false)
		defer restore()
		_ = submitAlertToPTY(w, harness, message, time.Sleep)
	}
	return pump
}

func (p *terminalSubsAlertPump) ObserveUserInput(now time.Time, input []byte) {
	p.sm.ObserveUserInput(now, input)
}

// ObserveSubsUnread is the source-side handle the forwardSubsAlerts
// loop calls each time SubsWait returns a reply with unread rows.
// The pump only needs to know "something is unread" — the row
// detail is for the agent's subsequent `ppz subs read`, not for
// the alert text — so this takes only `now`.
func (p *terminalSubsAlertPump) ObserveSubsUnread(now time.Time) {
	p.sm.ObserveSubsUnread(now)
}

// ObserveSubsUnreadSnapshot forwards the wakeup with its row state so
// the state machine can seed the pre-injection consumption baseline.
func (p *terminalSubsAlertPump) ObserveSubsUnreadSnapshot(now time.Time, reply cliproto.ListReply) {
	p.sm.ObserveSubsUnreadSnapshot(now, reply)
}

func (p *terminalSubsAlertPump) Flush(now time.Time) bool {
	alert := p.sm.ReadyAlert(now)
	if alert == "" {
		return false
	}
	p.BeginAlertMode(now)
	p.write(alert)
	p.EndAlertMode(now)
	return true
}

// submitAlertToPTY writes the alert message followed by a
// harness-appropriate Enter-equivalent into w. Claude Code reads
// `\x1b[13u` (kitty keyboard protocol Enter) as a single key event,
// so we send the message and terminator in one write. Every other
// harness's REPL needs a plain `\r` to submit — but only when the CR
// arrives slightly after the message bytes: copilot and codex were
// observed treating the CR as a literal newline inside the line
// rather than a submit when it shipped in the same write burst as
// the message. `ppz command -cr` already uses a 100ms pause between
// the message and the CR (see cmdCommand at command.go:93) and works
// reliably on both harnesses, so we mirror that pattern.
//
// sleep is injected so tests can verify the pause happened without
// blocking the test process. Production callers pass time.Sleep.
//
// Empty/unknown harness — non-agent `ppz terminal share` calls
// where PPZ_AGENT_HARNESS is unset, or a harness we haven't yet
// confirmed — falls into the `\r`+pause arm: a plain carriage return
// is the lowest-risk default since most line-discipline REPLs accept
// it as Enter, and the pause never hurts a REPL that would have
// accepted CR in the same burst.
func submitAlertToPTY(w io.Writer, harness, message string, sleep func(time.Duration)) error {
	trimmed := strings.TrimRight(message, "\r\n")
	if harness == "claude" {
		_, err := io.WriteString(w, trimmed+"\x1b[13u")
		return err
	}
	if _, err := io.WriteString(w, trimmed); err != nil {
		return err
	}
	sleep(100 * time.Millisecond)
	_, err := io.WriteString(w, "\r")
	return err
}

func (p *terminalSubsAlertPump) BeginAlertMode(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.alert = true
}

func (p *terminalSubsAlertPump) EndAlertMode(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.alert = false
	if len(p.buffer) > 0 {
		_, _ = p.pty.Write(p.buffer)
		p.buffer = nil
	}
}

func (p *terminalSubsAlertPump) ForwardUserInput(now time.Time, input []byte) bool {
	if len(input) == 0 {
		return true
	}
	p.ObserveUserInput(now, input)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.alert {
		p.buffer = append(p.buffer, input...)
		return false
	}
	_, _ = p.pty.Write(input)
	return true
}
