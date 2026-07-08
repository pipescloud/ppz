package cliproto

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// senderColumnWidth is the fixed-width column used to keep the body
// column aligned across messages in streaming output. Handle regex caps
// at 32 chars; long senders overflow the column rather than getting
// truncated, on the theory that a misaligned line beats a lost identity.
const senderColumnWidth = 12

// ANSI palette for the tabular read renderer, applied only on the
// interactive-TTY path (the caller gates via shouldUseColor). Timestamp
// renders dim — it's secondary metadata — and the sender bold-cyan so the
// two header fields visually separate from the white message body. Pinned
// by TestFormatReadMessage_ColorsTimestampAndSender.
const (
	readAnsiReset  = "\x1b[0m"
	readAnsiTime   = "\x1b[2m"    // dim
	readAnsiSender = "\x1b[1;36m" // bold cyan
)

// minBodyColumnWidth is the floor below which FormatReadMessage stops
// word-wrapping. On a very narrow terminal — or when a long sender overflows
// the sender column and eats the prefix budget — wrapping to a handful of
// columns produces one-word-per-line sludge; better to emit the body
// unwrapped and let the terminal soft-wrap it.
const minBodyColumnWidth = 20

// IsTabularReadPipe reports whether `ppz read <handle>.<pipe>` should
// render in the v0.23 three-column tabular form by default. Only the
// human-visible inbox-shaped pipes do — pty pipes (stdout / stdin /
// stdctrl) and user-named custom pipes stay byte-faithful so replays
// work and arbitrary tooling on top doesn't get reflowed bytes.
func IsTabularReadPipe(pipe string) bool {
	return pipe == "inbox" || pipe == "broadcast"
}

// FormatReadMessage renders one ReadMessage in the tabular default. Layout:
//
//	HH:MM:SS  <sender-col>  <body>
//	                        <continuation>
//	                        <continuation>
//
// Body shape:
//   - Subject starting with "ack:" → "<subject> → <last-8-hex-of-id>"
//     (system protocol message — payload is ignored for display).
//   - Subject otherwise non-empty → "[<subject>] <payload-line-1>".
//   - Subject empty → "<payload-line-1>".
//
// Multi-line payloads emit subsequent lines indented under the body column
// (no time, no sender). loc controls the timezone of HH:MM:SS — production
// callers pass time.Local.
//
// width is the terminal column budget used to word-wrap long body lines
// under the body column (0 disables wrapping — split on \n only). color
// toggles ANSI colour on the timestamp + sender fields; the caller gates
// it on TTY / NO_COLOR. (Both currently unused — wired in below.)
func FormatReadMessage(w io.Writer, m ReadMessage, loc *time.Location, width int, color bool) {
	t := parseCreatedAt(m.CreatedAt)
	if loc == nil {
		loc = time.UTC
	}
	timeCol := t.In(loc).Format("15:04:05")

	sender := m.Sender
	if sender == "" {
		sender = "-"
	}

	// Compute the body — subject-aware. For ack:* subjects, payload is
	// not displayed; the row is purely "ack:read → <id8>".
	body := bodyForRow(m)

	// The uncoloured prefix fixes column alignment and the continuation
	// indent. ANSI escapes are zero-width on the terminal, so everything
	// after the first line must reuse this *visible* width — never len() of
	// a coloured string.
	visiblePrefix := fmt.Sprintf("%s  %-*s  ", timeCol, senderColumnWidth, sender)
	prefixWidth := len(visiblePrefix)
	indent := strings.Repeat(" ", prefixWidth)

	// Colour the timestamp + sender on the first line only. Padding stays
	// outside the escapes so the visible prefix width is unchanged.
	firstPrefix := visiblePrefix
	if color {
		pad := senderColumnWidth - len(sender)
		if pad < 0 {
			pad = 0
		}
		firstPrefix = fmt.Sprintf("%s%s%s  %s%s%s%s  ",
			readAnsiTime, timeCol, readAnsiReset,
			readAnsiSender, sender, readAnsiReset, strings.Repeat(" ", pad))
	}

	// Word-wrap each logical line to the body column when there's a usable
	// width budget; otherwise emit as-is (split on \n only). The agent /
	// pipe path (width 0) and the too-narrow path both fall through here,
	// keeping their output reflow-free.
	bodyWidth := width - prefixWidth
	var rendered []string
	for _, line := range strings.Split(body, "\n") {
		if width > 0 && bodyWidth >= minBodyColumnWidth {
			rendered = append(rendered, wrapBodyLine(line, bodyWidth)...)
		} else {
			rendered = append(rendered, line)
		}
	}

	for i, line := range rendered {
		if i == 0 {
			fmt.Fprintln(w, firstPrefix+line)
		} else {
			fmt.Fprintln(w, indent+line)
		}
	}
}

// wrapBodyLine greedily word-wraps a single logical line to at most width
// columns, preserving any leading indentation (bullets, quoted blocks) on
// the first emitted sub-line. Lines already within width — the common case —
// pass through untouched so their internal spacing is preserved byte-for-
// byte. A single word longer than width is left to overflow its row rather
// than being hard-split mid-token. width math is byte-based, so multi-byte
// runes wrap slightly conservatively; the renderer never slices mid-word.
func wrapBodyLine(line string, width int) []string {
	if width <= 0 || len(line) <= width {
		return []string{line}
	}
	trimmed := strings.TrimLeft(line, " ")
	lead := line[:len(line)-len(trimmed)]
	words := strings.Fields(trimmed)
	if len(words) == 0 {
		return []string{line}
	}
	out := make([]string, 0, len(words))
	cur := lead + words[0]
	for _, word := range words[1:] {
		if len(cur)+1+len(word) <= width {
			cur += " " + word
			continue
		}
		out = append(out, cur)
		cur = word
	}
	return append(out, cur)
}

// bodyForRow returns the rendered body — the third column of one row,
// pre-split. ack:* subjects squash to "<subject> → <id8>"; non-ack
// subjects prepend "[<subject>] " to the first payload line.
//
// Explicit high/low priorities prepend a `P1 ` / `P3 ` badge (never for
// ack rows; never for unset/medium, keeping pre-priority rows byte-
// identical). The badge is advisory and human-only — it shares this text
// column with sender-controlled payload, so a payload beginning "P1 "
// renders indistinguishably. Agents must trust the structured `priority`
// field (--json) or delivery order, never the inline text.
func bodyForRow(m ReadMessage) string {
	if strings.HasPrefix(m.Subject, "ack:") {
		return fmt.Sprintf("%s → %s", m.Subject, lastHexOfID(m.ID, 8))
	}
	badge := ""
	if m.Priority == PriorityHigh || m.Priority == PriorityLow {
		badge = fmt.Sprintf("P%d ", m.Priority)
	}
	if m.Subject != "" {
		return badge + "[" + m.Subject + "] " + m.Payload
	}
	return badge + m.Payload
}

// lastHexOfID returns the last n hex characters of id, treating the id
// as a UUID-shaped string (dashes stripped). For a malformed / shorter
// id it returns the whole stripped value.
func lastHexOfID(id string, n int) string {
	stripped := strings.ReplaceAll(id, "-", "")
	if len(stripped) <= n {
		return stripped
	}
	return stripped[len(stripped)-n:]
}

// parseCreatedAt parses the daemon's emitted timestamp. The daemon
// formats with "2006-01-02T15:04:05Z" which is a strict subset of
// RFC3339 (no fractional, always Z). Fall back to time.Time{} on
// parse error — the formatter then renders 00:00:00, surfacing the
// problem rather than swallowing it.
func parseCreatedAt(s string) time.Time {
	if t, err := time.Parse("2006-01-02T15:04:05Z", s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
