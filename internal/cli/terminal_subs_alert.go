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
	// Known limitation: a read that lands DURING a long backoff window
	// is not observed until that window opens, because ConfirmUnread
	// only runs at fire time. If a new message arrives first, the
	// confirm sees unread and the ladder keeps climbing — so an agent
	// that recovered mid-window can wait up to CooldownMax for the
	// nudge on its next message. Bounded and self-healing (the first
	// gate-open with a clear level resets it), and CooldownMax is the
	// designed worst case anyway, so this is accepted rather than
	// fixed with an extra off-schedule poll.
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
	// CooldownMax) and is reset only by a negative ConfirmUnread —
	// the unread level actually dropping. Suppressed and deferred
	// fires inject nothing and so never increment it.
	unacked int
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
	if confirm != nil {
		reply, err := confirm()
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
	s.pending = false
	s.lastAlert = now
	s.unacked++
	return s.cfg.Message
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
