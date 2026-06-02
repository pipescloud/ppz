package cliproto

import (
	"strings"
	"testing"
)

// The `ppz send` delivery contract (2026-05-28) introduced two new error
// codes that the CLI must never surface generically. Their wire strings,
// exit codes, and messages are pinned here so a regression (e.g. someone
// reusing E_NATS_UNREACHABLE for unconfirmed delivery) is caught.

func TestEDeliveryUnconfirmed_StableSurface(t *testing.T) {
	if string(EDeliveryUnconfirmed) != "E_DELIVERY_UNCONFIRMED" {
		t.Fatalf("EDeliveryUnconfirmed wire string = %q, want E_DELIVERY_UNCONFIRMED", EDeliveryUnconfirmed)
	}
	if got := ExitCode(EDeliveryUnconfirmed); got != 25 {
		t.Errorf("ExitCode(EDeliveryUnconfirmed) = %d, want 25", got)
	}
	if msg := Message(EDeliveryUnconfirmed); msg == "" || msg == "unknown error" {
		t.Errorf("Message(EDeliveryUnconfirmed) is missing or generic: %q", msg)
	}
	if e := New(EDeliveryUnconfirmed); e == nil || e.Code != EDeliveryUnconfirmed {
		t.Fatalf("New(EDeliveryUnconfirmed) returned %+v", e)
	}
}

func TestEDaemonTimeout_StableSurface(t *testing.T) {
	if string(EDaemonTimeout) != "E_DAEMON_TIMEOUT" {
		t.Fatalf("EDaemonTimeout wire string = %q, want E_DAEMON_TIMEOUT", EDaemonTimeout)
	}
	if got := ExitCode(EDaemonTimeout); got != 26 {
		t.Errorf("ExitCode(EDaemonTimeout) = %d, want 26", got)
	}
	if msg := Message(EDaemonTimeout); msg == "" || msg == "unknown error" {
		t.Errorf("Message(EDaemonTimeout) is missing or generic: %q", msg)
	}
	if e := New(EDaemonTimeout); e == nil || e.Code != EDaemonTimeout {
		t.Fatalf("New(EDaemonTimeout) returned %+v", e)
	}
}

// v0.25.0: a new error code E_INVALID_SUBJECT for subject-rule violations
// (the `ack:` prefix is reserved for daemon-emitted protocol messages).
// CLI-side and daemon-side IPC both use it — wire string MUST be stable.
func TestEInvalidSubject_StableSurface(t *testing.T) {
	if string(EInvalidSubject) != "E_INVALID_SUBJECT" {
		t.Fatalf("EInvalidSubject wire string = %q, want E_INVALID_SUBJECT", EInvalidSubject)
	}
	if got := ExitCode(EInvalidSubject); got == 1 {
		t.Errorf("ExitCode(EInvalidSubject) = 1 (generic); want a dedicated non-1 exit code")
	}
	if msg := Message(EInvalidSubject); msg == "" || msg == "unknown error" {
		t.Errorf("Message(EInvalidSubject) is missing or generic: %q", msg)
	}
	// Constructor returns a populated *Error.
	e := New(EInvalidSubject)
	if e == nil || e.Code != EInvalidSubject {
		t.Fatalf("New(EInvalidSubject) returned %+v", e)
	}
}

// Track B (docs/AGENT_HARDENING.md): error-message accuracy. The
// E_INVALID_PIPE catalog default currently enumerates only the four
// built-in pipes ({broadcast, inbox, stdin, stdout}) as valid — but
// custom pipes created via `ppz pipe create` are also valid. MoltHub's
// Charlie and Bob both hit this and concluded that custom pipes were
// not supported.
//
// Properties asserted (RED today):
//   - Message MUST NOT contain the false enumerated set "{broadcast,
//     inbox, stdin, stdout}" — that's the misleading bit.
//   - Message MUST mention `ppz pipe create` (or equivalent — the
//     point is to point users at the actionable next step when they
//     hit this on a custom pipe).
//
// The exact wording is not pinned: the implementer is free to choose
// any phrasing that satisfies both properties.
func TestMessage_EInvalidPipe_DoesNotMisleadOnCustomPipes(t *testing.T) {
	msg := Message(EInvalidPipe)
	if strings.Contains(msg, "{broadcast, inbox, stdin, stdout}") {
		t.Errorf("Message(EInvalidPipe) still enumerates only built-in pipes:\n  %q\nthis misleads users into thinking custom pipes are invalid (MoltHub feedback, docs/AGENT_HARDENING.md Track B).", msg)
	}
	if !strings.Contains(msg, "ppz pipe create") {
		t.Errorf("Message(EInvalidPipe) should mention 'ppz pipe create' so users hitting this on a custom pipe see the actionable command:\n  %q", msg)
	}
}

// Track B (docs/AGENT_HARDENING.md): the E_NATS_UNREACHABLE catalog
// default currently suggests setting PPZ_NATS_URL when running
// outside docker — but the most-common cause MoltHub hit was
// expired credentials, not URL misconfiguration. Alice quoted:
// "the daemon was running and ppz status showed 'logged in,' but
// ppz ls threw E_NATS_UNREACHABLE."
//
// Properties asserted (RED today):
//   - Message MUST mention credentials / auth / login as a possible
//     cause (any of those tokens — implementer's choice). Currently
//     the message has none of them, so this fails.
//   - Existing PPZ_NATS_URL guidance is fine to keep, just no longer
//     the only suggestion. Not asserted here so the implementer can
//     drop it if they prefer a shorter message.
//
// The exact wording is not pinned.
func TestMessage_ENATSUnreachable_MentionsCredentialCause(t *testing.T) {
	msg := Message(ENATSUnreachable)
	credTokens := []string{"credential", "credentials", "login", "logout", "auth", "expired"}
	hasCredHint := false
	for _, tok := range credTokens {
		if strings.Contains(strings.ToLower(msg), tok) {
			hasCredHint = true
			break
		}
	}
	if !hasCredHint {
		t.Errorf("Message(ENATSUnreachable) should mention credentials / login / auth / expired as a possible cause (MoltHub feedback, docs/AGENT_HARDENING.md Track B); got:\n  %q", msg)
	}
}
