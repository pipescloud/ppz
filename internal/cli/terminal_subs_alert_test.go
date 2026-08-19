package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// subsUnreadReply builds the minimal fire-time snapshot the ConfirmUnread
// fakes return: one inbox pipe with the given unread level and total
// (retained) count. Total-Unread is the consumption watermark the
// backoff-reset logic derives, so tests that model "never reads" hold
// both constant and tests that model reads advance Total-Unread.
func subsUnreadReply(unread, total uint64) cliproto.ListReply {
	return cliproto.ListReply{Sources: []cliproto.Source{{
		Handle:    "zif",
		PipeInfos: []cliproto.PipeInfo{{Pipe: "inbox", Unread: unread, Total: total}},
	}}}
}

// confirmAlwaysUnread is the "agent never reads, no traffic" fake: a
// constant one-unread snapshot whose watermark never moves.
func confirmAlwaysUnread() (cliproto.ListReply, error) {
	return subsUnreadReply(1, 1), nil
}

func TestTerminalSubsAlertStateMachineDefersWhileUserActive(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	sm := newTerminalSubsAlertStateMachine(terminalSubsAlertConfig{
		IdleAfter: 15 * time.Second,
		Message:   terminalSubsAlertMessage,
	})

	sm.ObserveUserInput(now, []byte("partial prompt"))
	sm.ObserveSubsUnread(now.Add(time.Second))

	if got := sm.ReadyAlert(now.Add(14 * time.Second)); got != "" {
		t.Fatalf("ReadyAlert while user active = %q, want empty", got)
	}
}

func TestTerminalSubsAlertStateMachineInjectsAfterIdle(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	sm := newTerminalSubsAlertStateMachine(terminalSubsAlertConfig{
		IdleAfter: 15 * time.Second,
		Message:   terminalSubsAlertMessage,
	})

	sm.ObserveUserInput(now, []byte("partial prompt"))
	sm.ObserveSubsUnread(now.Add(time.Second))

	got := sm.ReadyAlert(now.Add(16 * time.Second))
	if !strings.Contains(got, "Please run 'ppz subs read' and action messages") {
		t.Fatalf("ReadyAlert after idle = %q, want subs alert text", got)
	}
	if !strings.Contains(got, "ppz subs read") {
		t.Fatalf("ReadyAlert after idle = %q, want ppz subs read guidance", got)
	}
}

func TestTerminalSubsAlertStateMachineCoalescesMultipleUnreadObservations(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	sm := newTerminalSubsAlertStateMachine(terminalSubsAlertConfig{
		IdleAfter: 15 * time.Second,
		Message:   terminalSubsAlertMessage,
	})

	sm.ObserveSubsUnread(now)
	sm.ObserveSubsUnread(now.Add(time.Second))
	sm.ObserveSubsUnread(now.Add(2 * time.Second))

	first := sm.ReadyAlert(now.Add(16 * time.Second))
	second := sm.ReadyAlert(now.Add(17 * time.Second))

	if first == "" {
		t.Fatal("first ReadyAlert is empty, want one coalesced alert")
	}
	if second != "" {
		t.Fatalf("second ReadyAlert = %q, want empty after coalescing", second)
	}
}

func TestTerminalSubsAlertPumpWritesClaudeSubmittedSubsAlertToPTYStdinAfterIdle(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter: 15 * time.Second,
		Message:   terminalSubsAlertMessage,
		Harness:   "claude",
	}, &ptyStdin)

	pump.ObserveUserInput(now, []byte("half typed command"))
	pump.ObserveSubsUnread(now.Add(time.Second))

	if wrote := pump.Flush(now.Add(14 * time.Second)); wrote {
		t.Fatalf("Flush before idle wrote alert to PTY stdin: %q", ptyStdin.String())
	}
	if ptyStdin.Len() != 0 {
		t.Fatalf("PTY stdin before idle = %q, want empty", ptyStdin.String())
	}

	if wrote := pump.Flush(now.Add(16 * time.Second)); !wrote {
		t.Fatal("Flush after idle did not write alert to PTY stdin")
	}
	got := ptyStdin.String()
	if !strings.HasPrefix(got, "Please run 'ppz subs read' and action messages") {
		t.Fatalf("PTY stdin alert = %q, want plain Claude instruction", got)
	}
	if !strings.HasSuffix(got, "\x1b[13u") {
		t.Fatalf("PTY stdin alert = %q, want Claude CSI-Enter-terminated instruction", got)
	}
	if strings.Contains(got, "ppz alert") {
		t.Fatalf("PTY stdin alert = %q, should not include ppz alert wrapper", got)
	}
	if wrote := pump.Flush(now.Add(17 * time.Second)); wrote {
		t.Fatalf("second Flush wrote duplicate alert to PTY stdin: %q", ptyStdin.String())
	}
}

func TestTerminalSubsAlertPumpCoalescesSubsObservationsIntoOnePTYAlert(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter: 15 * time.Second,
		Message:   terminalSubsAlertMessage,
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	pump.ObserveSubsUnread(now.Add(time.Second))
	pump.ObserveSubsUnread(now.Add(2 * time.Second))

	if wrote := pump.Flush(now.Add(16 * time.Second)); !wrote {
		t.Fatal("Flush after idle did not write coalesced alert")
	}
	first := ptyStdin.String()
	if strings.Count(first, "Please run 'ppz subs read' and action messages") != 1 {
		t.Fatalf("PTY stdin after coalesced alert = %q, want exactly one alert", first)
	}
	if wrote := pump.Flush(now.Add(17 * time.Second)); wrote {
		t.Fatalf("second Flush wrote duplicate coalesced alert: %q", ptyStdin.String())
	}
}

func TestTerminalSubsAlertPumpBuffersUserInputDuringAlertMode(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter: 15 * time.Second,
		Message:   terminalSubsAlertMessage,
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	pump.BeginAlertMode(now.Add(16 * time.Second))

	if forwarded := pump.ForwardUserInput(now.Add(16*time.Second), []byte("typed during alert")); forwarded {
		t.Fatalf("ForwardUserInput returned true during alert mode; user input should be buffered")
	}
	if strings.Contains(ptyStdin.String(), "typed during alert") {
		t.Fatalf("PTY stdin received user input during alert mode: %q", ptyStdin.String())
	}

	pump.EndAlertMode(now.Add(17 * time.Second))
	if !strings.Contains(ptyStdin.String(), "typed during alert") {
		t.Fatalf("PTY stdin after alert mode = %q, want buffered user input flushed", ptyStdin.String())
	}
}

// TestSubmitAlertToPTY_Claude pins the claude path: kitty keyboard
// protocol Enter (`\x1b[13u`) is a single key-event escape, so
// claude's REPL submits whatever input is on the line when it sees
// the escape regardless of whether the bytes arrived in one write
// or several. No pause is needed — and adding one would slow every
// claude alert by 100ms for zero benefit. The test injects a
// recording sleeper to verify it's never called on the claude path.
func TestSubmitAlertToPTY_Claude(t *testing.T) {
	var buf bytes.Buffer
	var sleeps []time.Duration
	if err := submitAlertToPTY(&buf, "claude", "hello\n", func(d time.Duration) {
		sleeps = append(sleeps, d)
	}); err != nil {
		t.Fatalf("submitAlertToPTY: %v", err)
	}
	if len(sleeps) != 0 {
		t.Errorf("claude path called sleep %d time(s) (%v); kitty Enter is a single key event, no pause needed", len(sleeps), sleeps)
	}
	if buf.String() != "hello\x1b[13u" {
		t.Errorf("claude buf=%q; want \"hello\\x1b[13u\" (message + kitty Enter, no trailing CR/LF before terminator)", buf.String())
	}
}

// TestSubmitAlertToPTY_NonClaude_PausesBeforeCarriageReturn pins
// the fix for the user-observed bug: with the message + `\r` in a
// single write burst, copilot and codex were treating the CR as a
// literal newline inside the line rather than as a submit. The
// working pattern in `ppz command -cr` (cmdCommand at
// command.go:93) writes the message, waits 100ms, then writes the
// CR — two writes with a pause between them. Mirror that here.
//
// The test injects a sleeper that snapshots the buffer at the
// moment sleep is called, so we can prove three things atomically:
//  1. The pause happens exactly once, at 100ms (matches cmdCommand).
//  2. The CR has NOT been written yet when sleep is called — the
//     message is on the wire alone, giving the REPL time to flush
//     it before the submit byte arrives.
//  3. The final buffer is message + `\r` (sequence preserved).
//
// Runs over every harness that takes the `\r` arm: known
// non-claude harnesses, plus empty (non-agent share) and a bogus
// string (forward-compat default).
func TestSubmitAlertToPTY_NonClaude_PausesBeforeCarriageReturn(t *testing.T) {
	for _, h := range []string{"copilot", "codex", "agy", "pi", "", "bogus"} {
		t.Run(h, func(t *testing.T) {
			var buf bytes.Buffer
			var snapshot string
			var sleeps []time.Duration
			if err := submitAlertToPTY(&buf, h, "hello\n", func(d time.Duration) {
				snapshot = buf.String()
				sleeps = append(sleeps, d)
			}); err != nil {
				t.Fatalf("submitAlertToPTY(%q): %v", h, err)
			}
			if len(sleeps) != 1 {
				t.Fatalf("harness %q: sleep called %d time(s); want exactly 1 (cmdCommand -cr uses one 100ms pause between message and CR; same pattern)", h, len(sleeps))
			}
			if sleeps[0] != 100*time.Millisecond {
				t.Errorf("harness %q: sleep duration=%v; want 100ms (matches cmdCommand at command.go:93)", h, sleeps[0])
			}
			if snapshot != "hello" {
				t.Errorf("harness %q: buffer at pause = %q; want \"hello\" (message only; CR must not be written until after the pause — otherwise copilot/codex bundle it with the message and treat it as a literal newline)", h, snapshot)
			}
			if buf.String() != "hello\r" {
				t.Errorf("harness %q: final buf=%q; want \"hello\\r\"", h, buf.String())
			}
		})
	}
}

// TestTerminalSubsAlertPump_CopilotHarness_UsesCarriageReturnSubmit
// pins the integration: the pump must thread cfg.Harness through
// to its write callback so the on-PTY bytes carry the
// harness-appropriate submit terminator. Without the wiring,
// configuring Harness: "copilot" would still produce the kitty
// escape and copilot's input buffer keeps showing the literal
// alert message — exactly the user-observed bug.
func TestTerminalSubsAlertPump_CopilotHarness_UsesCarriageReturnSubmit(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter: 15 * time.Second,
		Message:   terminalSubsAlertMessage,
		Harness:   "copilot",
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	if wrote := pump.Flush(now.Add(16 * time.Second)); !wrote {
		t.Fatal("Flush after idle did not write alert")
	}
	got := ptyStdin.String()
	if !strings.HasPrefix(got, "Please run 'ppz subs read' and action messages") {
		t.Errorf("PTY stdin alert = %q, want plain alert text prefix", got)
	}
	if !strings.HasSuffix(got, "\r") {
		t.Errorf("PTY stdin alert = %q, want trailing `\\r` (copilot's REPL submits on carriage return; kitty Enter would leave the alert literal in the input buffer)", got)
	}
	if strings.Contains(got, "\x1b[13u") {
		t.Errorf("PTY stdin alert = %q, must not contain claude's kitty Enter escape on copilot harness", got)
	}
}

func TestTerminalSubsAlertPumpCooldownSuppressesImmediateRepeatedAlerts(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	// ConfirmUnread always-true: the message is genuinely never read in
	// this scenario, so the fire-time gate must not change the re-nag
	// cadence — repeated alerts per cooldown window stay correct.
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:     15 * time.Second,
		Cooldown:      30 * time.Second,
		Message:       terminalSubsAlertMessage,
		ConfirmUnread: confirmAlwaysUnread,
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	if wrote := pump.Flush(now.Add(16 * time.Second)); !wrote {
		t.Fatal("first Flush did not write alert")
	}

	pump.ObserveSubsUnread(now.Add(17 * time.Second))
	if wrote := pump.Flush(now.Add(20 * time.Second)); wrote {
		t.Fatalf("Flush during cooldown wrote repeated alert: %q", ptyStdin.String())
	}

	if wrote := pump.Flush(now.Add(47 * time.Second)); !wrote {
		t.Fatal("Flush after cooldown did not write pending repeated alert")
	}
	if strings.Count(ptyStdin.String(), "Please run 'ppz subs read' and action messages") != 2 {
		t.Fatalf("PTY stdin after cooldown = %q, want two total alerts", ptyStdin.String())
	}
}

// The four tests below pin the fire-time confirmation gate — the fix
// for the redundant-final-nag bug that survives #119.
//
// Bug shape: streamForwardSubsAlertsOnce re-arms pending every ~250ms
// while a message sits unread (level-triggered subs wait). The agent's
// `ppz subs read` advances the cursor but publishes nothing on a
// subscribed subject, so the in-flight subs wait BLOCKS rather than
// returning the empty reply ObserveSubsClear (#119) listens for. A
// pending bit armed within 250ms before the read therefore survives
// it, and once idle + cooldown pass, the pump injects one final nag
// for a message that was already handled.
//
// Design rule the gate encodes: never act on cached level state —
// re-sample unread at the moment of injection.

func TestTerminalSubsAlertPumpSuppressesAlertWhenUnreadGoneAtFireTime(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	unreadNow := true
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter: 15 * time.Second,
		Cooldown:  30 * time.Second,
		Message:   terminalSubsAlertMessage,
		ConfirmUnread: func() (cliproto.ListReply, error) {
			if !unreadNow {
				return cliproto.ListReply{}, nil
			}
			return subsUnreadReply(1, 1), nil
		},
	}, &ptyStdin)

	// Message lands; subs wait loop arms pending. Before the idle gate
	// opens, the agent reads it — unread level drops to zero, but no
	// down-edge reaches the pump.
	pump.ObserveSubsUnread(now)
	unreadNow = false

	if wrote := pump.Flush(now.Add(16 * time.Second)); wrote {
		t.Fatalf("Flush fired for already-read message: %q, want fire-time confirm to suppress", ptyStdin.String())
	}
	if ptyStdin.Len() != 0 {
		t.Fatalf("PTY stdin after suppressed fire = %q, want empty", ptyStdin.String())
	}

	// The negative confirm must CLEAR pending, not just skip: if the
	// stale bit survives, the next tick re-fires the moment the level
	// goes high again for an unrelated reason.
	unreadNow = true
	if wrote := pump.Flush(now.Add(17 * time.Second)); wrote {
		t.Fatalf("stale pending survived a negative confirm and re-fired: %q", ptyStdin.String())
	}

	// A genuinely new unread observation must still alert — and on the
	// normal schedule. A suppressed (phantom) fire must not have
	// stamped lastAlert: if it had, the 30s cooldown would defer this
	// real alert to 46s.
	pump.ObserveSubsUnread(now.Add(18 * time.Second))
	if wrote := pump.Flush(now.Add(34 * time.Second)); !wrote {
		t.Fatal("Flush after new unread did not alert; suppressed fire must not consume the cooldown")
	}
	if strings.Count(ptyStdin.String(), "Please run 'ppz subs read' and action messages") != 1 {
		t.Fatalf("PTY stdin = %q, want exactly one alert (for the new message only)", ptyStdin.String())
	}
}

func TestTerminalSubsAlertPumpFiresWhenUnreadConfirmedAtFireTime(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:     15 * time.Second,
		Cooldown:      30 * time.Second,
		Message:       terminalSubsAlertMessage,
		ConfirmUnread: confirmAlwaysUnread,
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	if wrote := pump.Flush(now.Add(16 * time.Second)); !wrote {
		t.Fatal("Flush with confirmed unread did not write alert")
	}
	if !strings.HasPrefix(ptyStdin.String(), "Please run 'ppz subs read' and action messages") {
		t.Fatalf("PTY stdin alert = %q, want alert text", ptyStdin.String())
	}
}

func TestTerminalSubsAlertPumpConsultsConfirmOnlyWhenOtherwiseReady(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	calls := 0
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:     15 * time.Second,
		Cooldown:      30 * time.Second,
		Message:       terminalSubsAlertMessage,
		ConfirmUnread: func() (cliproto.ListReply, error) { calls++; return subsUnreadReply(1, 1), nil },
	}, &ptyStdin)

	// Not pending: the confirm IPC must not run on every 1s flush tick
	// for the lifetime of an idle share.
	pump.Flush(now)
	if calls != 0 {
		t.Fatalf("confirm consulted %d time(s) with nothing pending, want 0", calls)
	}

	// Pending but inside the idle gate: still no consult.
	pump.ObserveSubsUnread(now)
	pump.Flush(now.Add(5 * time.Second))
	if calls != 0 {
		t.Fatalf("confirm consulted %d time(s) before idle gate opened, want 0", calls)
	}

	// All gates open: exactly one consult, and the alert fires.
	if wrote := pump.Flush(now.Add(16 * time.Second)); !wrote {
		t.Fatal("Flush after idle did not write alert")
	}
	if calls != 1 {
		t.Fatalf("confirm consulted %d time(s) at fire, want exactly 1", calls)
	}

	// Pending re-armed but inside cooldown: no consult.
	pump.ObserveSubsUnread(now.Add(17 * time.Second))
	pump.Flush(now.Add(20 * time.Second))
	if calls != 1 {
		t.Fatalf("confirm consulted %d time(s) during cooldown, want 1", calls)
	}
}

// TestTerminalSubsAlertPumpDefersWhenUserTypesDuringConfirm pins the
// post-confirm re-check in ReadyAlert: the confirm IPC runs outside
// the state-machine lock, so user input can arrive while it's in
// flight. If it does, the idle gate has closed and the alert must NOT
// inject into the middle of whatever the user started typing — but it
// must be DEFERRED, not dropped: pending survives the re-check, so the
// alert fires on a later tick once the user goes idle again.
func TestTerminalSubsAlertPumpDefersWhenUserTypesDuringConfirm(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	var pump *terminalSubsAlertPump
	pump = newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter: 15 * time.Second,
		Cooldown:  30 * time.Second,
		Message:   terminalSubsAlertMessage,
		ConfirmUnread: func() (cliproto.ListReply, error) {
			// Keystrokes land mid-confirm, after the gates were checked.
			pump.ObserveUserInput(now.Add(16*time.Second), []byte("typed during confirm"))
			return subsUnreadReply(1, 1), nil
		},
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	if wrote := pump.Flush(now.Add(16 * time.Second)); wrote {
		t.Fatalf("Flush injected despite user input during confirm: %q, want deferred", ptyStdin.String())
	}
	if ptyStdin.Len() != 0 {
		t.Fatalf("PTY stdin after deferred fire = %q, want empty", ptyStdin.String())
	}

	// Deferred, not dropped: with the user idle again (and no lastAlert
	// stamped by the deferral), the same pending alert fires without a
	// fresh unread observation.
	if wrote := pump.Flush(now.Add(40 * time.Second)); !wrote {
		t.Fatal("Flush after user went idle again did not fire the deferred alert")
	}
	if strings.Count(ptyStdin.String(), "Please run 'ppz subs read' and action messages") != 1 {
		t.Fatalf("PTY stdin = %q, want exactly one (deferred) alert", ptyStdin.String())
	}
}

// TestConfirmSubsUnreadDecision pins the error semantics of the
// production wiring: only a positive "nothing unread" suppresses. An
// IPC failure (daemon restarting mid-share, socket hiccup) maps to
// fire-anyway — the nag is at-least-once; a redundant alert for a
// just-read message is annoying, a silently swallowed alert for an
// unread one loses a message.
func TestConfirmSubsUnreadDecision(t *testing.T) {
	unread := cliproto.ListReply{Sources: []cliproto.Source{{
		Handle:    "bob",
		PipeInfos: []cliproto.PipeInfo{{Pipe: "inbox", Unread: 1}},
	}}}
	if !confirmSubsUnreadDecision(unread, nil) {
		t.Error("decision(unread rows, nil err) = false, want true (fire)")
	}
	if confirmSubsUnreadDecision(cliproto.ListReply{}, nil) {
		t.Error("decision(no unread, nil err) = true, want false (suppress)")
	}
	if !confirmSubsUnreadDecision(cliproto.ListReply{}, errors.New("ipc: connection refused")) {
		t.Error("decision(zero reply, err) = false, want true (cannot disprove unread → fire)")
	}
}

// The ForPTY pump must route every byte it injects into the PTY —
// alert submissions and buffered user-input flushes alike — through
// the provided writer, not the raw master file. Production passes
// harnessInputWriter there so injected bytes taint detection's output
// causality exactly like local keystrokes; writing to the raw master
// instead would let the alert's echo read as untainted agent output
// and flash `ppz who` to working (docs/specs/agent-detection.md).
// The master fd itself is still needed for echo suppression, which is
// why the writer is a separate parameter.
func TestTerminalSubsAlertPumpForPTY_InjectsThroughProvidedWriter(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty open unavailable in this environment: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	var rec bytes.Buffer
	p := newTerminalSubsAlertPumpForPTY(terminalSubsAlertConfig{Harness: "claude"}, ptmx, &rec)

	p.write("nudge: subs unread")
	if !strings.Contains(rec.String(), "nudge: subs unread") {
		t.Errorf("alert submission bypassed the provided writer; writer saw %q", rec.String())
	}

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	p.BeginAlertMode(now)
	p.ForwardUserInput(now, []byte("typed-during-alert"))
	p.EndAlertMode(now)
	if !strings.Contains(rec.String(), "typed-during-alert") {
		t.Errorf("buffered user-input flush bypassed the provided writer; writer saw %q", rec.String())
	}
}

// ---------------------------------------------------------------------
// Backoff ladder: bound the alert flood when nagging isn't working.
//
// Bug (user-observed): the cooldown is a FIXED interval, so an unread
// message the agent cannot action produces one injected+submitted turn
// every cooldown window, forever. The reported trigger is a session
// usage limit: the agent hits its limit with a message unread, the
// wrapper nags every 30s for the n hours until the limit resets, and
// the whole backlog then flushes as real turns — burning context window
// and tokens. At the old 30s cadence a 5-hour block queues ~600 copies
// of the same nag.
//
// Nothing in the pump distinguishes "first nudge" from "600th nudge":
// ConfirmUnread correctly reports the message is still unread every
// single time, so it green-lights all of them, and the subs-wait loop
// re-arms pending every ~250ms regardless.
//
// Design rule these tests encode: a repeat alert is evidence the
// previous one did not work, so repeats back off geometrically. The
// ladder rung is the count of alerts INJECTED since the last proof the
// agent consumed anything — and the only such proof is a negative
// ConfirmUnread (the unread level actually dropped). Message arrivals
// are not proof of anything and must never reset the ladder, or a
// limit-blocked agent receiving traffic floods exactly as before.
// ---------------------------------------------------------------------

// TestTerminalSubsAlertPumpBacksOffWhileAlertsGoUnacknowledged walks
// the ladder rung by rung, asserting each gap both ways: no fire one
// second early, fire exactly on time. Gaps double from the base until
// they hit the ceiling, then stay there.
func TestTerminalSubsAlertPumpBacksOffWhileAlertsGoUnacknowledged(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		// Never read: every fire-time confirm says "still unread", so
		// the ladder is the ONLY thing bounding the injection count.
		ConfirmUnread: confirmAlwaysUnread,
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	if wrote := pump.Flush(now.Add(59 * time.Second)); wrote {
		t.Fatalf("Flush before the 60s idle gate fired: %q", ptyStdin.String())
	}
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("first nudge did not fire at the 60s idle gate")
	}

	// Gaps after alert N: min(5m * 2^(N-1), 30m).
	for i, gap := range []time.Duration{
		5 * time.Minute,
		10 * time.Minute,
		20 * time.Minute,
		30 * time.Minute, // ceiling reached
		30 * time.Minute, // ceiling held
		30 * time.Minute, // ceiling held
	} {
		// The subs-wait loop re-arms pending within ~250ms of every
		// fire while the message stays unread; mirror that here so the
		// cooldown gate is provably what defers, not a stale pending.
		pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))

		if wrote := pump.Flush(at.Add(gap - time.Second)); wrote {
			t.Fatalf("rung %d: alert fired 1s early; want the gap to have grown to %v", i+1, gap)
		}
		at = at.Add(gap)
		if wrote := pump.Flush(at); !wrote {
			t.Fatalf("rung %d: no alert after the expected %v gap", i+1, gap)
		}
	}

	if got := strings.Count(ptyStdin.String(), "Please run 'ppz subs read' and action messages"); got != 7 {
		t.Fatalf("injected %d alerts across the ladder, want 7 (1 nudge + 6 rungs)", got)
	}
}

// TestTerminalSubsAlertPumpLadderBoundsInjectionsAcrossUsageLimitBlock
// is the direct regression test for the reported symptom: an agent
// blocked for 5 hours with one unread message.
//
// Simulates the production loop exactly — a 1s flush tick, pending
// re-armed every tick by the level-triggered subs wait, and a confirm
// that truthfully reports "still unread" throughout, because the agent
// genuinely cannot run `ppz subs read`.
//
// Old fixed-30s behaviour: ~600 injected turns, all of which flush into
// the model the instant the limit lifts. With the ladder the schedule
// is 1m, 6m, 16m, 36m, then every 30m — 12 alerts across the block.
func TestTerminalSubsAlertPumpLadderBoundsInjectionsAcrossUsageLimitBlock(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   defaultSubsAlertIdleAfter,
		Cooldown:    defaultSubsAlertCooldown,
		CooldownMax: defaultSubsAlertCooldownMax,
		Message:     terminalSubsAlertMessage,
		// claude harness: its submit terminator needs no inter-write
		// pause, so 18000 simulated ticks don't drag a real 100ms
		// sleep per injection into the test's wall clock.
		Harness:       "claude",
		ConfirmUnread: confirmAlwaysUnread,
	}, &ptyStdin)

	const block = 5 * time.Hour
	for elapsed := time.Duration(0); elapsed < block; elapsed += time.Second {
		at := now.Add(elapsed)
		pump.ObserveSubsUnread(at) // level-triggered re-arm, every tick
		pump.Flush(at)             // production flush ticker: 1s
	}

	got := strings.Count(ptyStdin.String(), "Please run 'ppz subs read' and action messages")
	if got != 12 {
		t.Fatalf("injected %d alerts across a 5h block, want 12 "+
			"(1m, 6m, 16m, 36m, then every 30m); the old fixed-30s cadence injected ~600", got)
	}
}

// TestTerminalSubsAlertPumpLadderResetsAfterConfirmedRead pins the
// reset condition. A negative ConfirmUnread is the one signal that
// proves the agent consumed its messages, so it must return the pump
// to the base cadence — otherwise a single stalled stretch permanently
// desensitises the nag for the rest of the share session.
func TestTerminalSubsAlertPumpLadderResetsAfterConfirmedRead(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	unread := true
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		ConfirmUnread: func() (cliproto.ListReply, error) {
			if !unread {
				return cliproto.ListReply{}, nil
			}
			return subsUnreadReply(1, 1), nil
		},
	}, &ptyStdin)

	// Climb three rungs: nudge at 60s, then +5m, then +10m.
	pump.ObserveSubsUnread(now)
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("first nudge did not fire")
	}
	for _, gap := range []time.Duration{5 * time.Minute, 10 * time.Minute} {
		pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
		at = at.Add(gap)
		if wrote := pump.Flush(at); !wrote {
			t.Fatalf("climb: no alert after %v gap", gap)
		}
	}
	// Next rung would be 20m.

	// The agent comes back and reads. The subs-wait loop's pending bit
	// is still armed (the cursor advance publishes nothing), so the
	// fire-time confirm is what observes the drop.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	unread = false
	if wrote := pump.Flush(at.Add(20 * time.Minute)); wrote {
		t.Fatalf("alert fired for an already-read message: %q", ptyStdin.String())
	}
	before := strings.Count(ptyStdin.String(), "Please run 'ppz subs read' and action messages")

	// A new message lands well after the read. It must be nudged on the
	// BASE cadence — 60s idle — not on the 20m rung the pump had
	// climbed to before the read.
	fresh := at.Add(30 * time.Minute)
	unread = true
	pump.ObserveSubsUnread(fresh)
	if wrote := pump.Flush(fresh.Add(60 * time.Second)); !wrote {
		// Deliberately does NOT claim to prove the reset: this fire is
		// 31m past lastAlert, which clears the un-reset 20m rung too,
		// so it cannot fail for that reason. The reset can only be
		// observed at a gate-open moment, which is by definition >= the
		// un-reset rung — the assertion below is what carries the claim.
		t.Fatal("post-read message was not nudged at the base 60s idle gate")
	}
	// And the rung after it is the base gap again, not a resumed climb.
	pump.ObserveSubsUnread(fresh.Add(60*time.Second + 250*time.Millisecond))
	if wrote := pump.Flush(fresh.Add(60*time.Second + 5*time.Minute)); !wrote {
		t.Fatal("second post-read alert did not fire after the base 5m gap; the ladder resumed mid-climb instead of resetting")
	}
	if got := strings.Count(ptyStdin.String(), "Please run 'ppz subs read' and action messages"); got != before+2 {
		t.Fatalf("injected %d alerts after the read, want 2", got-before)
	}
}

// TestTerminalSubsAlertPumpLadderNotResetByNewUnreadMessages is the
// crux of the fix. During a usage-limit block the agent keeps
// RECEIVING messages while being unable to action any of them. If
// arrivals reset the ladder, a busy pipe reproduces the original flood
// in full — the ladder would never climb past its first rung.
//
// Only consumption resets. Arrival is not consumption.
func TestTerminalSubsAlertPumpLadderNotResetByNewUnreadMessages(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:     60 * time.Second,
		Cooldown:      5 * time.Minute,
		CooldownMax:   30 * time.Minute,
		Message:       terminalSubsAlertMessage,
		ConfirmUnread: confirmAlwaysUnread,
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("first nudge did not fire")
	}
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	at = at.Add(5 * time.Minute)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("second alert did not fire after the base 5m gap")
	}
	// Two alerts delivered, unacknowledged: the next gap is 10m.

	// A burst of genuinely new messages arrives mid-block — each one a
	// fresh unread observation from the subs-wait loop.
	for i := 1; i <= 20; i++ {
		pump.ObserveSubsUnread(at.Add(time.Duration(i) * 10 * time.Second))
	}

	if wrote := pump.Flush(at.Add(5 * time.Minute)); wrote {
		t.Fatalf("new arrivals reset the ladder to the base 5m gap: %q; "+
			"arrivals are not proof the agent consumed anything, and resetting on them "+
			"reproduces the original flood on any busy pipe", ptyStdin.String())
	}
	if wrote := pump.Flush(at.Add(10*time.Minute - time.Second)); wrote {
		t.Fatal("alert fired 1s before the 10m rung")
	}
	if wrote := pump.Flush(at.Add(10 * time.Minute)); !wrote {
		t.Fatal("no alert at the 10m rung; the ladder must keep climbing across arrivals")
	}
	if got := strings.Count(ptyStdin.String(), "Please run 'ppz subs read' and action messages"); got != 3 {
		t.Fatalf("injected %d alerts, want 3 despite 20 message arrivals", got)
	}
}

// TestTerminalSubsAlertPumpDeferredFireDoesNotAdvanceLadder pins the
// accounting boundary: the rung counts alerts actually INJECTED, not
// fire attempts. A deferral (user typed while the confirm was in
// flight) injects nothing, so it must not consume a rung — otherwise
// a user typing at the wrong moment silently doubles the delay before
// the agent is ever nudged.
func TestTerminalSubsAlertPumpDeferredFireDoesNotAdvanceLadder(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	var pump *terminalSubsAlertPump
	deferOnce := true
	pump = newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		ConfirmUnread: func() (cliproto.ListReply, error) {
			if deferOnce {
				deferOnce = false
				// Keystrokes land after the gates were checked, closing
				// the idle gate before the injection happens.
				pump.ObserveUserInput(now.Add(60*time.Second), []byte("typed mid-confirm"))
			}
			return subsUnreadReply(1, 1), nil
		},
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	if wrote := pump.Flush(now.Add(60 * time.Second)); wrote {
		t.Fatalf("deferral injected anyway: %q", ptyStdin.String())
	}

	// User goes idle; the deferred alert lands. This is rung 1.
	at := now.Add(3 * time.Minute)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("deferred alert never fired once the user went idle")
	}

	// The gap after it must be the BASE 5m, not the 10m rung a
	// rung-consuming deferral would have left behind.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	if wrote := pump.Flush(at.Add(5 * time.Minute)); !wrote {
		t.Fatal("next alert did not fire after the base 5m gap; the deferral consumed a ladder rung despite injecting nothing")
	}
}

// TestTerminalSubsAlertCooldownMaxDefaultsAboveBase guards the
// degenerate configs. An unset ceiling must not clamp the ladder to
// zero growth (that silently restores the flood), and a ceiling below
// the base must not shrink the first gap below the configured base.
func TestTerminalSubsAlertCooldownMaxDefaultsAboveBase(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	// IdleAfter is deliberately shorter than every gap here: a re-armed
	// pending restamps pendingSince, so the idle gate is re-applied to
	// each repeat and would mask the cooldown gate under test if it
	// were the larger of the two. Production never hits this (60s idle
	// vs a 5m base gap).
	t.Run("unset ceiling still backs off", func(t *testing.T) {
		var ptyStdin bytes.Buffer
		pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
			IdleAfter:     10 * time.Second,
			Cooldown:      30 * time.Second,
			Message:       terminalSubsAlertMessage,
			ConfirmUnread: confirmAlwaysUnread,
		}, &ptyStdin)

		pump.ObserveSubsUnread(now)
		at := now.Add(10 * time.Second)
		if wrote := pump.Flush(at); !wrote {
			t.Fatal("first nudge did not fire")
		}
		pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
		at = at.Add(30 * time.Second)
		if wrote := pump.Flush(at); !wrote {
			t.Fatal("second alert did not fire after the base 30s gap")
		}
		pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
		if wrote := pump.Flush(at.Add(30 * time.Second)); wrote {
			t.Fatal("third alert fired after another flat 30s; an unset ceiling must still let the ladder climb")
		}
		if wrote := pump.Flush(at.Add(60 * time.Second)); !wrote {
			t.Fatal("third alert did not fire after the doubled 60s gap")
		}
	})

	t.Run("ceiling below base does not shrink the base gap", func(t *testing.T) {
		var ptyStdin bytes.Buffer
		pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
			IdleAfter:     60 * time.Second,
			Cooldown:      5 * time.Minute,
			CooldownMax:   time.Minute, // misconfiguration: below base
			Message:       terminalSubsAlertMessage,
			ConfirmUnread: confirmAlwaysUnread,
		}, &ptyStdin)

		pump.ObserveSubsUnread(now)
		at := now.Add(60 * time.Second)
		if wrote := pump.Flush(at); !wrote {
			t.Fatal("first nudge did not fire")
		}
		pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
		if wrote := pump.Flush(at.Add(5 * time.Minute)); !wrote {
			t.Fatal("no alert after the configured 5m base gap")
		}
		at = at.Add(5 * time.Minute)

		// Rung 2 is where the ceiling is actually consulted: rung 1
		// short-circuits on `unacked <= 1` and returns the base gap
		// without ever reading CooldownMax, so probing the
		// misconfiguration at rung 1 proves nothing. 70s, not 60s: the
		// re-arm restamps pendingSince 250ms late, so a 60s probe is
		// still inside the idle gate and would mask the cooldown gate
		// under test.
		pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
		if wrote := pump.Flush(at.Add(70 * time.Second)); wrote {
			t.Fatalf("rung 2 collapsed onto the sub-base 1m ceiling: %q; the base gap is a floor", ptyStdin.String())
		}
		if wrote := pump.Flush(at.Add(5 * time.Minute)); !wrote {
			t.Fatal("rung 2 did not fire after the 5m base gap")
		}
	})
}

// TestTerminalSubsAlertDefaults pins the production cadence decision
// itself, so changing it is a deliberate edit to a named constant with
// a failing test attached rather than an inline tweak at the call site.
//
// 60s first nudge: wide enough that an agent's own subs watch usually
// reads the message first (the fire-time confirm then suppresses the
// nudge entirely and nothing reaches the PTY), prompt enough that a
// genuinely parked agent isn't sitting on an unread message for
// minutes. 5m base / 30m ceiling: see the 5h-block bound above.
func TestTerminalSubsAlertDefaults(t *testing.T) {
	if defaultSubsAlertIdleAfter != 60*time.Second {
		t.Errorf("defaultSubsAlertIdleAfter = %v, want 60s", defaultSubsAlertIdleAfter)
	}
	if defaultSubsAlertCooldown != 5*time.Minute {
		t.Errorf("defaultSubsAlertCooldown = %v, want 5m", defaultSubsAlertCooldown)
	}
	if defaultSubsAlertCooldownMax != 30*time.Minute {
		t.Errorf("defaultSubsAlertCooldownMax = %v, want 30m", defaultSubsAlertCooldownMax)
	}

	// A zero-value config must land on the production cadence: the
	// state machine is the single source of the defaults, so a caller
	// that forgets a field cannot silently get the old 15s/no-backoff
	// behaviour back.
	sm := newTerminalSubsAlertStateMachine(terminalSubsAlertConfig{})
	if sm.cfg.IdleAfter != defaultSubsAlertIdleAfter {
		t.Errorf("zero-config IdleAfter = %v, want %v", sm.cfg.IdleAfter, defaultSubsAlertIdleAfter)
	}
	if sm.cfg.Cooldown != defaultSubsAlertCooldown {
		t.Errorf("zero-config Cooldown = %v, want %v", sm.cfg.Cooldown, defaultSubsAlertCooldown)
	}
	if sm.cfg.CooldownMax != defaultSubsAlertCooldownMax {
		t.Errorf("zero-config CooldownMax = %v, want %v", sm.cfg.CooldownMax, defaultSubsAlertCooldownMax)
	}
}

// ---------------------------------------------------------------------
// Consumption-watermark ladder reset: the fix for the field-observed
// bug where the backoff climbed monotonically for a fully responsive
// agent.
//
// Observed live on 2026-08-19 (host with the zif share): alerts at
// 11:08:01, 11:13:01, 11:23:02, 11:43:03 — gaps 5m, 10m, 20m — while
// the agent read and replied to every message BETWEEN alerts. The
// ladder never saw a single read.
//
// Root cause: the reset lives only in the negative-ConfirmUnread
// branch, which triggers when a fire attempt catches the unread level
// at exactly zero. Fire attempts only happen at gate-open moments,
// minutes apart. A read is therefore invisible unless NO new message
// arrives before the next gate opens — under conversation-style
// traffic there always is one, the confirm sees "unread", and unacked
// increments. The design rule "a repeat alert is evidence the previous
// one did not work" inverts: the repeat fires for a message that
// arrived seconds ago while the previous alert's episode was fully
// consumed.
//
// Fix these tests encode: consumption is observed via a per-pipe
// WATERMARK — Total-Unread — computed from the snapshot ConfirmUnread
// already returns. At each injection the state machine records the
// watermarks; at the next fire attempt, if any pipe's watermark
// advanced, the agent consumed since the last alert, so the ladder
// resets and the fire counts as a fresh episode's first alert.
//
//   - arrival:            Total+1, Unread+1 → watermark unchanged
//   - read (full/partial): Unread drops     → watermark ADVANCES
//   - unread msg expires:  both drop        → watermark unchanged
//   - read msg expires:    Total drops      → watermark DECREASES
//
// Only an advance resets. Arrivals-never-reset (pinned above) is
// preserved for free.
// ---------------------------------------------------------------------

// TestTerminalSubsAlertPumpLadderResetsWhenReadOccludedByNewArrival is
// the direct regression test for the live sequence. Each cycle: alert
// fires → agent reads everything → a NEW message lands before the next
// gate-open. The fire-time confirm always sees "unread", so the old
// level-only reset never triggers — but the watermark advanced every
// cycle, so every alert must fire on the BASE gap. On the buggy build
// alert #3 waits 10m and #4 waits 20m.
func TestTerminalSubsAlertPumpLadderResetsWhenReadOccludedByNewArrival(t *testing.T) {
	now := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	// Scripted fire-time snapshots. One confirm per injection:
	//   alert #1: msg A unread              (1 unread / 1 total, wm 0)
	//   alert #2: A read, B arrived         (1/2, wm 1 — ADVANCED)
	//   alert #3: B read, C arrived         (1/3, wm 2 — ADVANCED)
	//   alert #4: C read, D arrived         (1/4, wm 3 — ADVANCED)
	replies := []cliproto.ListReply{
		subsUnreadReply(1, 1),
		subsUnreadReply(1, 2),
		subsUnreadReply(1, 3),
		subsUnreadReply(1, 4),
	}
	confirms := 0
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		Harness:     "claude",
		ConfirmUnread: func() (cliproto.ListReply, error) {
			r := replies[min(confirms, len(replies)-1)]
			confirms++
			return r, nil
		},
	}, &ptyStdin)

	// msg A lands; first nudge at the 60s idle gate.
	pump.ObserveSubsUnread(now)
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #1 did not fire at the 60s idle gate")
	}

	for cycle := 2; cycle <= 4; cycle++ {
		// The subs-wait loop re-arms pending ~250ms after the fire
		// (previous message still unread), the agent then reads it, and
		// the next message arrives — all before the gate reopens. The
		// read itself produces no observation (the down-edge is
		// invisible); it is encoded solely in the next confirm's
		// watermark.
		pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))

		if wrote := pump.Flush(at.Add(5*time.Minute - time.Second)); wrote {
			t.Fatalf("alert #%d fired 1s before the base gap", cycle)
		}
		at = at.Add(5 * time.Minute)
		if wrote := pump.Flush(at); !wrote {
			t.Fatalf("alert #%d did not fire on the BASE 5m gap; the ladder climbed despite the agent consuming every prior message (unacked never reset)", cycle)
		}
	}

	if got := strings.Count(ptyStdin.String(), "Please run 'ppz subs read' and action messages"); got != 4 {
		t.Fatalf("injected %d alerts, want 4", got)
	}
	if confirms != 4 {
		t.Fatalf("confirm consulted %d times, want 4 (once per injection)", confirms)
	}
}

// TestTerminalSubsAlertPumpPartialReadCountsAsConsumption pins the
// interaction with the read flood cap: `subs read` delivers at most 10
// unread per invocation, so an agent draining a spammed pipe pages
// through it — Unread drops without reaching zero. That IS consumption
// and must reset the ladder, even though the level never goes clear.
// It also pins the converse: once the watermark stops advancing, the
// climb resumes.
func TestTerminalSubsAlertPumpPartialReadCountsAsConsumption(t *testing.T) {
	now := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	replies := []cliproto.ListReply{
		subsUnreadReply(15, 15), // alert #1: 15 unread, nothing consumed
		subsUnreadReply(5, 15),  // alert #2: one head-10 page read — wm 0→10
		subsUnreadReply(5, 15),  // alert #3: no further reads — wm still 10
		subsUnreadReply(5, 15),  // alert #4: no further reads
	}
	confirms := 0
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		Harness:     "claude",
		ConfirmUnread: func() (cliproto.ListReply, error) {
			r := replies[min(confirms, len(replies)-1)]
			confirms++
			return r, nil
		},
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #1 did not fire")
	}

	// Agent pages 10 of 15 between alerts; alert #2 on the base gap.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	at = at.Add(5 * time.Minute)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #2 did not fire after the base gap")
	}

	// The partial read advanced the watermark, so #2 was a fresh
	// episode's first alert: #3 must come on the BASE gap again.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	at = at.Add(5 * time.Minute)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #3 did not fire on the base gap; a paged (partial) read must count as consumption and reset the ladder")
	}

	// No reads since #2's snapshot: the climb resumes — #4 waits the
	// doubled gap.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	if wrote := pump.Flush(at.Add(5 * time.Minute)); wrote {
		t.Fatal("alert #4 fired on the base gap; with no watermark advance since #3 the ladder must resume climbing")
	}
	if wrote := pump.Flush(at.Add(10 * time.Minute)); !wrote {
		t.Fatal("alert #4 did not fire after the doubled 10m gap")
	}
}

// TestTerminalSubsAlertPumpUncollaredPipeReadResetsLadder keeps the
// watermark walk honest across BOTH row families in a ListReply:
// subsReplyHasUnread walks Sources[].PipeInfos and UncollaredPipes[]
// — consumption on an uncollared room the agent subscribed via
// `ppz subs add` must reset exactly like an inbox read.
func TestTerminalSubsAlertPumpUncollaredPipeReadResetsLadder(t *testing.T) {
	now := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	uncollared := func(unread, total uint64) cliproto.ListReply {
		return cliproto.ListReply{UncollaredPipes: []cliproto.UncollaredPipe{{
			Name: "warroom",
			Info: cliproto.PipeInfo{Pipe: "warroom", Unread: unread, Total: total},
		}}}
	}
	replies := []cliproto.ListReply{
		uncollared(1, 1), // alert #1
		uncollared(1, 2), // alert #2: read + new arrival — wm 0→1
		uncollared(1, 3), // alert #3: read + new arrival — wm 1→2
	}
	confirms := 0
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		Harness:     "claude",
		ConfirmUnread: func() (cliproto.ListReply, error) {
			r := replies[min(confirms, len(replies)-1)]
			confirms++
			return r, nil
		},
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #1 did not fire")
	}
	for cycle := 2; cycle <= 3; cycle++ {
		pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
		at = at.Add(5 * time.Minute)
		if wrote := pump.Flush(at); !wrote {
			t.Fatalf("alert #%d did not fire on the base gap; uncollared-pipe consumption must reset the ladder like an inbox read", cycle)
		}
	}
}

// The three guards below pass on the CURRENT (buggy) build too — they
// are not reproductions, they pin the boundaries of the fix so the
// watermark comparison cannot overshoot into resetting on the wrong
// signals. Flagged here so a reviewer doesn't count them as RED.

// TestTerminalSubsAlertPumpArrivalsMoveTotalsButNotWatermark: arrivals
// grow Total and Unread in lockstep, watermark stays put — the ladder
// must keep climbing. This is the arrivals-never-reset property
// re-pinned against the watermark implementation specifically: a naive
// "totals changed → activity → reset" heuristic dies here.
func TestTerminalSubsAlertPumpArrivalsMoveTotalsButNotWatermark(t *testing.T) {
	now := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	replies := []cliproto.ListReply{
		subsUnreadReply(1, 1), // alert #1
		subsUnreadReply(2, 2), // alert #2: one more arrived, none read — wm 0
		subsUnreadReply(3, 3), // alert #3: same — wm 0
	}
	confirms := 0
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		Harness:     "claude",
		ConfirmUnread: func() (cliproto.ListReply, error) {
			r := replies[min(confirms, len(replies)-1)]
			confirms++
			return r, nil
		},
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #1 did not fire")
	}
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	at = at.Add(5 * time.Minute)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #2 did not fire after the base gap")
	}
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	if wrote := pump.Flush(at.Add(5 * time.Minute)); wrote {
		t.Fatal("alert #3 fired on the base gap; arrivals moved the totals but consumed nothing — the ladder must keep climbing")
	}
	if wrote := pump.Flush(at.Add(10 * time.Minute)); !wrote {
		t.Fatal("alert #3 did not fire after the doubled 10m gap")
	}
}

// TestTerminalSubsAlertPumpWatermarkDecreaseDoesNotReset: retention
// expiry of already-read messages shrinks Total without touching
// Unread, so the watermark DROPS. A drop is not consumption; the
// ladder must keep climbing. (An implementation comparing |wm_now -
// wm_then| != 0, or signed wrongly, dies here.)
func TestTerminalSubsAlertPumpWatermarkDecreaseDoesNotReset(t *testing.T) {
	now := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	replies := []cliproto.ListReply{
		subsUnreadReply(1, 5), // alert #1: wm 4
		subsUnreadReply(1, 3), // alert #2: two read msgs expired — wm 2 (DOWN)
	}
	confirms := 0
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		Harness:     "claude",
		ConfirmUnread: func() (cliproto.ListReply, error) {
			r := replies[min(confirms, len(replies)-1)]
			confirms++
			return r, nil
		},
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #1 did not fire")
	}
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	at = at.Add(5 * time.Minute)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #2 did not fire after the base gap")
	}
	// wm went 4→2: not an advance. Ladder at unacked=2 → 10m gap.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	if wrote := pump.Flush(at.Add(5 * time.Minute)); wrote {
		t.Fatal("alert #3 fired on the base gap; a watermark DECREASE (expiry of read messages) is not consumption and must not reset the ladder")
	}
}

// TestTerminalSubsAlertPumpNewPipeDoesNotResetLadder: a pipe absent
// from the last snapshot (the agent ran `ppz subs add` mid-flight)
// arrives carrying a nonzero watermark of its own. That history
// predates the subscription; it proves nothing about consumption since
// the last alert. Only pipes present in BOTH snapshots participate.
func TestTerminalSubsAlertPumpNewPipeDoesNotResetLadder(t *testing.T) {
	now := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	withNewPipe := cliproto.ListReply{Sources: []cliproto.Source{{
		Handle: "zif",
		PipeInfos: []cliproto.PipeInfo{
			{Pipe: "inbox", Unread: 1, Total: 1},   // unchanged, wm 0
			{Pipe: "warroom", Unread: 1, Total: 9}, // NEW pipe, wm 8 of pre-subscription history
		},
	}}}
	replies := []cliproto.ListReply{
		subsUnreadReply(1, 1), // alert #1: inbox only
		withNewPipe,           // alert #2: inbox unchanged + freshly subscribed room
	}
	confirms := 0
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		Harness:     "claude",
		ConfirmUnread: func() (cliproto.ListReply, error) {
			r := replies[min(confirms, len(replies)-1)]
			confirms++
			return r, nil
		},
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #1 did not fire")
	}
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	at = at.Add(5 * time.Minute)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #2 did not fire after the base gap")
	}
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	if wrote := pump.Flush(at.Add(5 * time.Minute)); wrote {
		t.Fatal("alert #3 fired on the base gap; a freshly subscribed pipe's pre-existing watermark is not consumption since the last alert")
	}
}

// TestTerminalSubsAlertPumpConfirmErrorNeitherSuppressesNorResets: the
// at-least-once rule survives the watermark change. An IPC failure at
// fire time still fires (cannot disprove unread), and — with no
// snapshot to compare — must not reset the ladder either: a daemon
// flapping for an hour must not hold the nag at base cadence.
func TestTerminalSubsAlertPumpConfirmErrorNeitherSuppressesNorResets(t *testing.T) {
	now := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	failing := true
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		Harness:     "claude",
		ConfirmUnread: func() (cliproto.ListReply, error) {
			if failing {
				return cliproto.ListReply{}, errors.New("ipc: connection refused")
			}
			return subsUnreadReply(1, 1), nil
		},
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #1 did not fire on confirm error; the nag is at-least-once")
	}
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	at = at.Add(5 * time.Minute)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #2 did not fire after the base gap")
	}
	// Two error-fires, no evidence of consumption: the ladder must be
	// at unacked=2 → 10m gap.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	if wrote := pump.Flush(at.Add(5 * time.Minute)); wrote {
		t.Fatal("alert #3 fired on the base gap; error-fires carry no consumption evidence and must not reset the ladder")
	}
	if wrote := pump.Flush(at.Add(10 * time.Minute)); !wrote {
		t.Fatal("alert #3 did not fire after the doubled 10m gap")
	}
}

// TestTerminalSubsAlertPumpDeferredFireStillObservesConsumption pins
// where the watermark baseline lives: it may only move at INJECTION
// time. A deferral (user typed mid-confirm) runs the confirm and gets
// a reply carrying the advance, but injects nothing — if that reply
// quietly re-baselines the snapshot without applying the reset, the
// consumption evidence is destroyed and the eventually-fired alert
// climbs the ladder despite a real read. Outcome pinned, not
// mechanism: whether the implementation resets during the deferred
// attempt or re-derives the advance at the later fire, the alert that
// eventually lands must be a fresh episode's first — base gap after.
func TestTerminalSubsAlertPumpDeferredFireStillObservesConsumption(t *testing.T) {
	now := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	var pump *terminalSubsAlertPump
	deferOnce := false
	confirms := 0
	pump = newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		Harness:     "claude",
		ConfirmUnread: func() (cliproto.ListReply, error) {
			confirms++
			if deferOnce {
				deferOnce = false
				pump.ObserveUserInput(now.Add(6*time.Minute), []byte("typed mid-confirm"))
			}
			if confirms == 1 {
				return subsUnreadReply(1, 1), nil // alert #1: msg A unread
			}
			return subsUnreadReply(1, 2), nil // A read, B arrived — wm 0→1
		},
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #1 did not fire")
	}

	// Agent reads A; msg B arrives; the base-gap fire attempt runs its
	// confirm (which sees the advance) but is deferred by keystrokes.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	deferOnce = true
	if wrote := pump.Flush(at.Add(5 * time.Minute)); wrote {
		t.Fatalf("deferral injected anyway: %q", ptyStdin.String())
	}

	// User idle again: the deferred alert lands.
	at = at.Add(8 * time.Minute)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("deferred alert never fired once the user went idle")
	}

	// The consumption that preceded the deferral must have been
	// credited: the fired alert is a fresh episode's first, so the
	// next repeat comes on the BASE gap, not a climbed rung.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	if wrote := pump.Flush(at.Add(5 * time.Minute)); !wrote {
		t.Fatal("next alert did not fire on the base gap; the deferred attempt's confirm reply destroyed the consumption evidence (baseline may only move at injection)")
	}
}

// TestTerminalSubsAlertPumpErrorFireKeepsConsumptionBaseline pins the
// snapshot behaviour across error-fires: an IPC failure at fire time
// yields no snapshot, so the LAST GOOD baseline must be retained — a
// read that happened around a flapping daemon is still credited at
// the next successful confirm. An implementation that clears the
// baseline on error would silently disable resets for one extra
// window after every daemon hiccup.
func TestTerminalSubsAlertPumpErrorFireKeepsConsumptionBaseline(t *testing.T) {
	now := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	confirms := 0
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		Harness:     "claude",
		ConfirmUnread: func() (cliproto.ListReply, error) {
			confirms++
			switch confirms {
			case 1:
				return subsUnreadReply(1, 1), nil // alert #1: baseline wm 0
			case 2:
				return cliproto.ListReply{}, errors.New("ipc: daemon restarting")
			default:
				return subsUnreadReply(1, 2), nil // read + new arrival — wm 1 vs baseline 0
			}
		},
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #1 did not fire")
	}

	// Error-fire: at-least-once, no snapshot taken.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	at = at.Add(5 * time.Minute)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #2 did not fire on confirm error")
	}

	// The agent read around the hiccup; a new message arrived. The
	// next successful confirm sees wm 1 vs the retained wm-0 baseline
	// from alert #1 → reset → alert #3 fires... but only after ITS
	// gate: alert #2 climbed to unacked=2, so the gate is 10m. The
	// reset is observable in what follows #3, not in #3's own timing.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	at = at.Add(10 * time.Minute)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #3 did not fire after the 10m rung")
	}

	// #3's confirm observed the advance → it was a fresh episode's
	// first alert → #4 comes on the BASE gap. A baseline-clearing
	// implementation sees no advance at #3 (nothing to compare), so
	// unacked climbs to 3 and #4 waits 20m.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	if wrote := pump.Flush(at.Add(5 * time.Minute)); !wrote {
		t.Fatal("alert #4 did not fire on the base gap; the error-fire dropped the consumption baseline instead of retaining it")
	}
}

// TestTerminalSubsAlertPumpUnreadAboveTotalDoesNotSpuriouslyReset
// guards the uint64 arithmetic: a daemon bug (or mid-update race)
// reporting Unread > Total makes the naive Total-Unread underflow to
// ~2^64, which reads as an enormous watermark advance and spuriously
// resets the ladder. The watermark must clamp at zero.
func TestTerminalSubsAlertPumpUnreadAboveTotalDoesNotSpuriouslyReset(t *testing.T) {
	now := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	var ptyStdin bytes.Buffer
	confirms := 0
	pump := newTerminalSubsAlertPump(terminalSubsAlertConfig{
		IdleAfter:   60 * time.Second,
		Cooldown:    5 * time.Minute,
		CooldownMax: 30 * time.Minute,
		Message:     terminalSubsAlertMessage,
		Harness:     "claude",
		ConfirmUnread: func() (cliproto.ListReply, error) {
			confirms++
			if confirms == 1 {
				return subsUnreadReply(1, 1), nil // baseline wm 0
			}
			return subsUnreadReply(5, 3), nil // inconsistent snapshot: underflow bait
		},
	}, &ptyStdin)

	pump.ObserveSubsUnread(now)
	at := now.Add(60 * time.Second)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #1 did not fire")
	}
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	at = at.Add(5 * time.Minute)
	if wrote := pump.Flush(at); !wrote {
		t.Fatal("alert #2 did not fire after the base gap")
	}
	// Nothing was consumed; the inconsistent snapshot must not have
	// read as an advance. Ladder at unacked=2 → 10m gap.
	pump.ObserveSubsUnread(at.Add(250 * time.Millisecond))
	if wrote := pump.Flush(at.Add(5 * time.Minute)); wrote {
		t.Fatal("alert #3 fired on the base gap; Unread > Total underflowed into a spurious watermark advance (clamp at zero)")
	}
}
