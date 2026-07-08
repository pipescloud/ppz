package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/daemon"
)

// sendOut / sendErr are package-level so tests can divert output to
// buffers. cmdSend writes its success line to sendErr (stderr in
// production) so scripts redirecting stdout — the Salt Town root cause
// for "missed the sent id=… line" — still see it. Stderr is a partial
// fix only: callers that combine streams (`&>file`, `2>&1 >file`) still
// swallow it. That's the explicit semantics of "redirect everything".
var (
	sendOut io.Writer = os.Stdout
	sendErr io.Writer = os.Stderr
)

// cmdSend: ppz send <handle>[.<pipe>] <payload>
//
//	[--subject <s>] [--in-reply-to <id>] [--request-ack] [--priority <p>]
//
// Bare handles target .inbox for direct source/agent messages. Explicit
// <handle>.<pipe> targets can write to .stdin of a running terminal pipe,
// or to .broadcast / custom pipes.
//
// v0.25.0 flags:
//   - --subject:      free-form subject (header-line metadata). The `ack:`
//     prefix is reserved for daemon-emitted system messages
//     and rejected at the CLI BEFORE the IPC call (handlers.go also
//     rejects at the trust boundary — belt + suspenders).
//   - --in-reply-to:  uuid of the message this one replies to. Renders as
//     a thread linkage in tabular `ppz read` output.
//   - --request-ack:  ask the recipient's daemon to auto-emit an
//     `ack:read` envelope back to the sender's inbox when their cursor
//     advances past this message. **Best-effort, non-blocking**:
//     a failed ack publish does not block cursor advancement, so the
//     sender sees no ack if either the recipient hasn't read yet OR the
//     ack publish itself failed (those are indistinguishable). Callers
//     wanting strict guarantees should layer their own re-send-on-
//     timeout. Requires a non-empty current source — preflighted
//     against IPCStatus before the broadcast IPC call.
//
// v0.51.0 flag:
//   - --priority:     delivery-order hint, 1|high 2|medium 3|low. The
//     recipient's inbox/broadcast drain delivers higher tiers first
//     (stable — FIFO within a tier). Omitted = unset (0 on the wire),
//     which readers treat as medium. Invalid values are rejected at the
//     CLI with E_INVALID_PRIORITY (handleSend re-checks at the IPC trust
//     boundary). Inert on byte-faithful pipes (stdout/stdin/stdctrl/
//     custom) and on `--tail` live streams — those stay arrival-ordered.
//
// Success line goes to STDERR (not stdout) so harnesses that redirect
// stdout still surface delivery confirmation. With --request-ack the
// line gains an ` ack=requested` token to remind the operator that the
// flag took effect.
func cmdSend(args []string) error {
	if wantsHelp(args) {
		printHelp(os.Stdout, "send")
		return nil
	}
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(sendErr)
	subject := fs.String("subject", "", "envelope-level subject; renders as `[subject] payload` in tabular read")
	inReplyTo := fs.String("in-reply-to", "", "uuid of the message this one replies to (sets envelope.in_reply_to)")
	requestAck := fs.Bool("request-ack", false, "ask the recipient's daemon to auto-emit ack:read on cursor advance (best-effort, non-blocking)")
	priorityFlag := fs.String("priority", "", "delivery-order hint: 1|high, 2|medium, 3|low; the recipient's inbox drain delivers higher first")
	target, payload, flagArgs, err := splitSendArgs(args)
	if err != nil {
		printHelp(sendErr, "send")
		os.Exit(2)
	}
	if err := fs.Parse(flagArgs); err != nil {
		os.Exit(2)
	}
	if target == "" || payload == "" {
		printHelp(sendErr, "send")
		os.Exit(2)
	}

	if strings.HasPrefix(*subject, "ack:") {
		return cliproto.New(cliproto.EInvalidSubject)
	}

	priority, err := parsePriority(*priorityFlag)
	if err != nil {
		return cliproto.New(cliproto.EInvalidPriority)
	}

	// Phase 1.5: capture the raw bare target before the .inbox sugar.
	// The daemon uses BareTarget to fall back to uncollared pipe
	// resolution when the source-handle lookup misses.
	bareTarget := ""
	if !strings.Contains(target, ".") {
		bareTarget = target
		target += ".inbox"
	}
	idx := strings.LastIndex(target, ".")
	if idx <= 0 || idx == len(target)-1 {
		return cliproto.New(cliproto.EInvalidPipe)
	}
	handle, channel := target[:idx], target[idx+1:]

	if *requestAck {
		// Preflight: --request-ack is only meaningful when the sender has
		// a current source — otherwise the receiver's daemon, even if
		// it does the ack auto-emit, has no destination to send the ack
		// back to. Better to reject early at the CLI than let the user
		// believe an ack will arrive.
		if _, err := effectiveCurrentHandle(); err != nil {
			return err
		}
	}

	var reply cliproto.SendReply
	if err := daemon.Call(ipcSocket(), cliproto.IPCSend,
		cliproto.SendRequest{
			Handle:       handle,
			Channel:      channel,
			BareTarget:   bareTarget,
			Payload:      payload,
			MsgSubject:   *subject,
			InReplyTo:    *inReplyTo,
			AckRequested: *requestAck,
			Priority:     priority,
			// Forward the calling shell's session id so the daemon's
			// envelope.sender = d.State.Current(req.Session) resolves
			// against the per-tty current source — not the "default"
			// session, which is rarely what the user actually has set.
			Session: sessionID(),
			// Forward PPZ_CURRENT_HANDLE as an explicit sender hint when
			// the env says we're inside a wrapped pty (terminalShareEnv
			// exports it). The daemon's IPCCreate skips SetCurrent for
			// PTY-kind sources so its own State.Current(session) would
			// otherwise be empty, even though `ppz status` /
			// `ppz read inbox` / the `--request-ack` preflight all
			// agree the wrapped handle is current via the same env var.
			// Empty hint preserves today's behaviour (daemon falls back
			// to State.Current) — see senderForRequest in sender_resolve.go.
			Sender: os.Getenv("PPZ_CURRENT_HANDLE"),
		},
		&reply); err != nil {
		return err
	}
	// `to=` shows the path the daemon actually published to. For
	// collared sends that's `<handle>.<pipe>`; for uncollared sends
	// (Phase 1.5) it's the manifold-prefixed leaf without a fake
	// `.inbox` suffix. Derived from reply.Subject (stripping the
	// account-id prefix) so the display always matches reality.
	display := stripAccountPrefix(reply.Subject, handle+"."+channel)
	printSendSuccess(sendErr, reply, display, *requestAck)
	return nil
}

// stripAccountPrefix returns the user-visible path from a full NATS
// subject by dropping the leading "<account_id>." token. Falls back to
// the supplied default when the subject doesn't carry an account-id
// prefix (e.g. older daemons that don't populate reply.Subject).
func stripAccountPrefix(subject, fallback string) string {
	idx := strings.Index(subject, ".")
	if idx <= 0 || idx == len(subject)-1 {
		return fallback
	}
	return subject[idx+1:]
}

// printSendSuccess writes the v0.25.0 success line. Output:
//
//	sent id=<id8> to=<subject> bytes=<n>
//	sent id=<id8> to=<subject> bytes=<n> ack=requested   # with --request-ack
//
// The `id` shown is the last 8 hex chars of the UUID for visual brevity;
// the full UUID stays in the message envelope (and in --json output if a
// future verb adds one).
func printSendSuccess(w io.Writer, r cliproto.SendReply, target string, ackRequested bool) {
	id8 := lastHex8(r.ID)
	if ackRequested {
		fmt.Fprintf(w, "sent id=%s to=%s bytes=%d ack=requested\n", id8, target, r.Bytes)
		return
	}
	fmt.Fprintf(w, "sent id=%s to=%s bytes=%d\n", id8, target, r.Bytes)
}

// lastHex8 returns the last 8 hex chars of a UUID-shaped id string,
// dashes stripped. For shorter / malformed ids it returns the whole
// stripped value.
func lastHex8(id string) string {
	stripped := strings.ReplaceAll(id, "-", "")
	if len(stripped) <= 8 {
		return stripped
	}
	return stripped[len(stripped)-8:]
}

// parsePriority maps the --priority flag value to the envelope tier:
// 1|high, 2|medium|med, 3|low (aliases case-insensitive). "" means the
// flag wasn't passed and returns 0 (unset — the daemon publishes it
// verbatim; readers treat 0 as medium). Explicit "0" is rejected: only
// omission means unset.
func parsePriority(s string) (int, error) {
	switch strings.ToLower(s) {
	case "":
		return 0, nil
	case "1", "high":
		return cliproto.PriorityHigh, nil
	case "2", "medium", "med":
		return cliproto.PriorityMedium, nil
	case "3", "low":
		return cliproto.PriorityLow, nil
	}
	return 0, fmt.Errorf("invalid priority %q", s)
}

// splitSendArgs takes the raw positional+flag mix and returns
// (target, payload, flagArgs). cmdSend takes two positional args (target,
// payload); flags can come before, between, or after them. We pull the
// first two positionals as target/payload and forward the rest to
// flag.Parse.
func splitSendArgs(args []string) (target, payload string, flagArgs []string, err error) {
	valueFlags := map[string]bool{
		"-subject":      true,
		"--subject":     true,
		"-in-reply-to":  true,
		"--in-reply-to": true,
		"-priority":     true,
		"--priority":    true,
	}
	positionals := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if strings.Contains(a, "=") || !valueFlags[a] {
				continue
			}
			if i+1 >= len(args) {
				return "", "", nil, fmt.Errorf("flag %s requires a value", a)
			}
			flagArgs = append(flagArgs, args[i+1])
			i++
			continue
		}
		switch positionals {
		case 0:
			target = a
		case 1:
			payload = a
		default:
			return "", "", nil, fmt.Errorf("unexpected extra positional %q", a)
		}
		positionals++
	}
	if positionals < 2 {
		return "", "", nil, fmt.Errorf("missing positional args")
	}
	return target, payload, flagArgs, nil
}
