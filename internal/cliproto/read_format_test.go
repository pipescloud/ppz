package cliproto

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ansiRe matches CSI colour sequences so tests can assert on the visible
// layout independent of the colour escapes layered on top.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripAnsi(s string) string { return ansiRe.ReplaceAllString(s, "") }

// FormatReadMessage is the v0.23 default `ppz read` renderer for inbox-shaped
// pipes. The bare opt-out and other modes (--json/--tty/--raw) bypass it.

func mkMsg(id, sender, subject, payload string) ReadMessage {
	return ReadMessage{
		ID:        id,
		Sender:    sender,
		Subject:   subject,
		Payload:   payload,
		CreatedAt: "2026-05-07T14:23:01Z",
	}
}

// IsTabularReadPipe identifies pipes that get the tabular default — only
// the human-visible pipes. Anything else (stdout / stdin / user-named)
// stays byte-faithful.
func TestIsTabularReadPipe(t *testing.T) {
	for _, pipe := range []string{"inbox", "broadcast"} {
		if !IsTabularReadPipe(pipe) {
			t.Errorf("IsTabularReadPipe(%q) = false, want true", pipe)
		}
	}
	for _, pipe := range []string{"stdout", "stdin", "stdctrl", "custom-name", ""} {
		if IsTabularReadPipe(pipe) {
			t.Errorf("IsTabularReadPipe(%q) = true, want false", pipe)
		}
	}
}

// Plain row: HH:MM:SS local time + sender + payload.
func TestFormatReadMessage_Plain(t *testing.T) {
	loc := time.FixedZone("test", 0) // UTC for stable test output
	var b bytes.Buffer
	FormatReadMessage(&b, mkMsg("11111111-2222-3333-4444-555566667777", "foo", "", "Hello, how are you?"), loc, 0, false)
	got := b.String()
	want := "14:23:01  foo           Hello, how are you?\n"
	if got != want {
		t.Errorf("plain row\n got: %q\nwant: %q", got, want)
	}
}

// Empty sender renders as "-".
func TestFormatReadMessage_EmptySenderDash(t *testing.T) {
	loc := time.FixedZone("test", 0)
	var b bytes.Buffer
	FormatReadMessage(&b, mkMsg("11111111-2222-3333-4444-555566667777", "", "", "this is another message"), loc, 0, false)
	got := b.String()
	if !strings.Contains(got, "  -  ") && !strings.Contains(got, "  -   ") {
		// Padded sender column starts with "-" then trailing space.
	}
	if !strings.HasPrefix(got, "14:23:01  -") {
		t.Errorf("empty-sender row should start with `14:23:01  -`, got %q", got)
	}
	if !strings.HasSuffix(got, "  this is another message\n") {
		t.Errorf("empty-sender row should end with payload, got %q", got)
	}
}

// ack:* subject becomes `<subject> → <last8-of-id-hex>`.
func TestFormatReadMessage_AckSubjectRendersWithLast8(t *testing.T) {
	loc := time.FixedZone("test", 0)
	var b bytes.Buffer
	// id hex (dashes stripped): 11112222333344445555666677778899 → last 8 = "77778899"
	FormatReadMessage(&b, mkMsg("11111111-2222-3333-4444-555566667777-8899", "miner-test", "ack:read", "(ignored payload)"), loc, 0, false)
	// Payload should NOT appear; the rendered body is the special form.
	got := b.String()
	if strings.Contains(got, "(ignored payload)") {
		t.Errorf("ack row leaked payload: %q", got)
	}
	if !strings.Contains(got, "ack:read → ") {
		t.Errorf("ack row missing arrow form: %q", got)
	}
	// Last 8 hex of stripped id "11112222333344445555666677778899" → "77778899"
	if !strings.Contains(got, "77778899") {
		t.Errorf("ack row missing last-8-hex of id: %q", got)
	}
}

// User-set non-ack subject renders inline as `[subject] payload`.
func TestFormatReadMessage_UserSubjectInline(t *testing.T) {
	loc := time.FixedZone("test", 0)
	var b bytes.Buffer
	FormatReadMessage(&b, mkMsg("aa", "smelter", "status update", "step 1 complete"), loc, 0, false)
	got := b.String()
	if !strings.HasSuffix(got, "  [status update] step 1 complete\n") {
		t.Errorf("user-subject row should render `[status update] step 1 complete`, got %q", got)
	}
}

// Multi-line payloads continue on indented subsequent lines aligned under the
// payload column (after the time + sender columns).
func TestFormatReadMessage_MultiLineIndentsContinuations(t *testing.T) {
	loc := time.FixedZone("test", 0)
	var b bytes.Buffer
	FormatReadMessage(&b, mkMsg("aa", "smelter", "", "status update:\nstep 1 complete\nstep 2 in progress"), loc, 0, false)
	got := b.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("multi-line want 3 lines, got %d: %q", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "14:23:01  smelter") {
		t.Errorf("first line should carry time+sender, got %q", lines[0])
	}
	// Continuation lines must NOT carry time or sender.
	for _, line := range lines[1:] {
		if strings.Contains(line, "14:23:01") {
			t.Errorf("continuation line leaked timestamp: %q", line)
		}
		if strings.Contains(line, "smelter") {
			t.Errorf("continuation line leaked sender: %q", line)
		}
		// Continuation indent should be at least the time-column width
		// so it lines up under <body>.
		if !strings.HasPrefix(line, "          ") { // ≥10 spaces
			t.Errorf("continuation line lacks indent: %q", line)
		}
	}
	// Body content preserved.
	if !strings.HasSuffix(lines[0], "status update:") {
		t.Errorf("first line body = %q, want ending `status update:`", lines[0])
	}
	if !strings.HasSuffix(lines[1], "step 1 complete") {
		t.Errorf("second line = %q", lines[1])
	}
	if !strings.HasSuffix(lines[2], "step 2 in progress") {
		t.Errorf("third line = %q", lines[2])
	}
}

// Continuation indent should align under the body column. Compute it via the
// known prefix shape so the test stays stable if column widths shift.
func TestFormatReadMessage_ContinuationIndentMatchesBodyColumn(t *testing.T) {
	loc := time.FixedZone("test", 0)
	var b bytes.Buffer
	FormatReadMessage(&b, mkMsg("aa", "x", "", "first\nsecond"), loc, 0, false)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	idx := strings.Index(lines[0], "first")
	if idx < 0 {
		t.Fatalf("body 'first' not found in line: %q", lines[0])
	}
	prefix := strings.Repeat(" ", idx)
	if !strings.HasPrefix(lines[1], prefix+"second") {
		t.Errorf("continuation indent mismatch — line[0] body at col %d, line[1]=%q", idx, lines[1])
	}
}

// Time renders in the supplied location (local in production).
func TestFormatReadMessage_TimeUsesProvidedLocation(t *testing.T) {
	tz := time.FixedZone("plus10", 10*3600)
	var b bytes.Buffer
	// CreatedAt is RFC3339-Z (UTC). +10h → 00:23:01 next day, but HH:MM:SS only shows time.
	FormatReadMessage(&b, mkMsg("aa", "x", "", "p"), tz, 0, false)
	if !strings.HasPrefix(b.String(), "00:23:01  ") {
		t.Errorf("time column should reflect +10h zone: %q", b.String())
	}
}

// bodyColWidth is the visible width of the time+sender prefix for a short
// sender, i.e. the column the body and every continuation line start at:
// "15:04:05" (8) + 2 + sender padded to senderColumnWidth (12) + 2 = 24.
const bodyColWidth = 8 + 2 + senderColumnWidth + 2

// stripPrefix removes the leading bodyColWidth columns (the time+sender
// prefix on line 0, or the indent on continuation lines), returning the
// body portion of a rendered row.
func stripPrefix(line string) string {
	if len(line) < bodyColWidth {
		return line
	}
	return line[bodyColWidth:]
}

// --- Intent: word-wrap long body lines under the body column (TTY only) ---

// A long single-line body wraps to the body column when a positive width
// budget is given. Continuation lines indent under the body (no time, no
// sender), no rendered line's body exceeds the body-column budget, and the
// words reassemble in order — wrapping must not lose or reorder content.
func TestFormatReadMessage_WrapsLongLineToBodyColumn(t *testing.T) {
	loc := time.FixedZone("test", 0)
	const width = 54 // bodyWidth = 54 - 24 = 30
	bodyWidth := width - bodyColWidth
	original := "This is a fairly long single line of text that must wrap across the body column cleanly"
	var b bytes.Buffer
	FormatReadMessage(&b, mkMsg("aa", "x", "", original), loc, width, false)

	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("long line should wrap to multiple rows, got %d:\n%s", len(lines), b.String())
	}
	if !strings.HasPrefix(lines[0], "14:23:01  x") {
		t.Errorf("first row should carry time+sender, got %q", lines[0])
	}
	var words []string
	for i, line := range lines {
		if i > 0 {
			// Continuation rows: indent exactly to the body column, no metadata.
			if !strings.HasPrefix(line, strings.Repeat(" ", bodyColWidth)) {
				t.Errorf("continuation row %d not indented to body column: %q", i, line)
			}
			if strings.Contains(line, "14:23:01") || strings.Contains(line, "  x ") {
				t.Errorf("continuation row %d leaked metadata: %q", i, line)
			}
		}
		body := stripPrefix(line)
		if len(body) > bodyWidth {
			t.Errorf("row %d body width %d exceeds budget %d: %q", i, len(body), bodyWidth, body)
		}
		words = append(words, strings.Fields(body)...)
	}
	if got := strings.Join(words, " "); got != original {
		t.Errorf("wrapped words do not reassemble to original\n got: %q\nwant: %q", got, original)
	}
}

// When the body column is too narrow to wrap usefully (a tiny terminal, or
// a long sender that has eaten the prefix budget), the renderer must NOT
// wrap into one-word-per-line sludge — it emits the body unwrapped and lets
// the terminal soft-wrap. A single \n-free body stays a single row.
func TestFormatReadMessage_NarrowWidthSkipsWrap(t *testing.T) {
	loc := time.FixedZone("test", 0)
	const width = 30 // bodyWidth = 6 — below the wrap floor
	original := "alpha bravo charlie delta echo foxtrot golf"
	var b bytes.Buffer
	FormatReadMessage(&b, mkMsg("aa", "x", "", original), loc, width, false)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("narrow width should not wrap, got %d rows:\n%s", len(lines), b.String())
	}
	if got := stripPrefix(lines[0]); got != original {
		t.Errorf("narrow-width body altered\n got: %q\nwant: %q", got, original)
	}
}

// Wrapping preserves leading indentation (bullets, quoted blocks) on the
// first emitted sub-line so list structure survives the reflow.
func TestFormatReadMessage_WrapPreservesLeadingIndent(t *testing.T) {
	loc := time.FixedZone("test", 0)
	const width = 54
	original := "  • bullet item that is quite long and should wrap onto another line"
	var b bytes.Buffer
	FormatReadMessage(&b, mkMsg("aa", "x", "", original), loc, width, false)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("indented bullet should wrap, got %d rows:\n%s", len(lines), b.String())
	}
	if got := stripPrefix(lines[0]); !strings.HasPrefix(got, "  • ") {
		t.Errorf("leading bullet indent lost on first sub-line: %q", got)
	}
}

// --- Intent: colour the timestamp + sender on the interactive TTY path ---

// ANSI palette pinned as the wire contract for coloured read rows. Tests
// assert these exact sequences so the colours don't drift silently.
const (
	wantTimeColor   = "\x1b[2m"    // dim — timestamp is secondary metadata
	wantSenderColor = "\x1b[1;36m" // bold cyan — sender stands out from body
	wantColorReset  = "\x1b[0m"
)

// With color enabled the timestamp and sender are wrapped in their ANSI
// codes; the body carries no escapes.
func TestFormatReadMessage_ColorsTimestampAndSender(t *testing.T) {
	loc := time.FixedZone("test", 0)
	var b bytes.Buffer
	FormatReadMessage(&b, mkMsg("aa", "smelter", "", "step 1 complete"), loc, 0, true)
	got := b.String()
	if !strings.Contains(got, wantTimeColor+"14:23:01"+wantColorReset) {
		t.Errorf("timestamp not colour-wrapped: %q", got)
	}
	if !strings.Contains(got, wantSenderColor+"smelter"+wantColorReset) {
		t.Errorf("sender not colour-wrapped: %q", got)
	}
	// Body must stay plain — no escape sequence after the sender's reset.
	_, after, _ := strings.Cut(got, "smelter"+wantColorReset)
	if strings.Contains(after, "\x1b[") {
		t.Errorf("body should carry no ANSI escapes, got %q", after)
	}
}

// Colour escapes are zero-width on the terminal, so they must not shift the
// body column. Stripping the ANSI from a coloured row must leave the body
// at exactly bodyColWidth, and continuation indents stay plain spaces of
// that width (the colour lives only on line 0's prefix).
func TestFormatReadMessage_ColorPreservesBodyColumnAlignment(t *testing.T) {
	loc := time.FixedZone("test", 0)
	var b bytes.Buffer
	FormatReadMessage(&b, mkMsg("aa", "x", "", "first\nsecond"), loc, 0, true)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 rows, got %d: %q", len(lines), b.String())
	}
	if idx := strings.Index(stripAnsi(lines[0]), "first"); idx != bodyColWidth {
		t.Errorf("coloured body starts at visible col %d, want %d: %q", idx, bodyColWidth, stripAnsi(lines[0]))
	}
	if lines[1] != strings.Repeat(" ", bodyColWidth)+"second" {
		t.Errorf("continuation row should be plain %d-space indent + body, got %q", bodyColWidth, lines[1])
	}
}

// --- Agent contract: non-interactive output is byte-identical to today ---

// The default path for a programmatic reader (pipe / agent / CI) passes
// width=0, color=false. A long line that WOULD wrap on a TTY must NOT wrap
// here, and no escape bytes may appear — the body is reflow-free so an agent
// can recover the original message from the rendered row. Regression guard
// against silently coupling the human-presentation reflow to this path.
func TestFormatReadMessage_NonInteractiveUnwrappedAndUncoloured(t *testing.T) {
	loc := time.FixedZone("test", 0)
	original := "This is a long single line that would certainly wrap on an interactive terminal but must not here"
	var b bytes.Buffer
	FormatReadMessage(&b, mkMsg("aa", "alice", "", original), loc, 0, false)
	got := b.String()
	if strings.Contains(got, "\x1b[") {
		t.Errorf("non-interactive output must carry no ANSI escapes: %q", got)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("non-interactive output must not wrap, got %d rows:\n%s", len(lines), got)
	}
	if body := stripPrefix(lines[0]); body != original {
		t.Errorf("non-interactive body altered\n got: %q\nwant: %q", body, original)
	}
}

// v0.51.0 priority badge: explicit high/low priorities prepend `P1 ` /
// `P3 ` to the body so a human scanning the tabular default sees them.
// Unset (0) and medium (2) render exactly as before — that invariant is
// what keeps every pre-priority golden fixture byte-identical.
//
// The badge is advisory and human-only: it shares the text column with
// sender-controlled payload (a payload could itself start with "P1 "),
// so agents must trust the structured `priority` field via --json, never
// the inline text. --json / --raw / --bare / --tty never see the badge —
// they bypass bodyForRow entirely (see the render switch in cli/read.go).
func TestFormatReadMessage_PriorityBadgeHighAndLow(t *testing.T) {
	loc := time.FixedZone("test", 0)

	high := mkMsg("aa", "foo", "", "drop everything")
	high.Priority = 1
	var b bytes.Buffer
	FormatReadMessage(&b, high, loc, 0, false)
	if !strings.HasSuffix(b.String(), "  P1 drop everything\n") {
		t.Errorf("high-priority row should carry P1 badge, got %q", b.String())
	}

	low := mkMsg("aa", "foo", "", "whenever")
	low.Priority = 3
	b.Reset()
	FormatReadMessage(&b, low, loc, 0, false)
	if !strings.HasSuffix(b.String(), "  P3 whenever\n") {
		t.Errorf("low-priority row should carry P3 badge, got %q", b.String())
	}
}

// Badge precedes the [subject] block: `P1 [subject] payload`.
func TestFormatReadMessage_PriorityBadgeBeforeSubject(t *testing.T) {
	loc := time.FixedZone("test", 0)
	m := mkMsg("aa", "foo", "status update", "step 1 done")
	m.Priority = 1
	var b bytes.Buffer
	FormatReadMessage(&b, m, loc, 0, false)
	if !strings.HasSuffix(b.String(), "  P1 [status update] step 1 done\n") {
		t.Errorf("badge should precede subject block, got %q", b.String())
	}
}

// Unset (0) and explicit medium (2) render byte-identically to a
// pre-priority row — no badge.
func TestFormatReadMessage_NoBadgeForUnsetAndMedium(t *testing.T) {
	loc := time.FixedZone("test", 0)
	want := "14:23:01  foo           Hello, how are you?\n"
	for _, p := range []int{0, 2} {
		m := mkMsg("11111111-2222-3333-4444-555566667777", "foo", "", "Hello, how are you?")
		m.Priority = p
		var b bytes.Buffer
		FormatReadMessage(&b, m, loc, 0, false)
		if got := b.String(); got != want {
			t.Errorf("priority %d row\n got: %q\nwant: %q (must be byte-identical to pre-priority output)", p, got, want)
		}
	}
}

// ack:* system rows never carry the badge, whatever the envelope says.
func TestFormatReadMessage_NoBadgeOnAckRows(t *testing.T) {
	loc := time.FixedZone("test", 0)
	m := mkMsg("11111111-2222-3333-4444-555566667777-8899", "miner-test", "ack:read", "")
	m.Priority = 1
	var b bytes.Buffer
	FormatReadMessage(&b, m, loc, 0, false)
	if strings.Contains(b.String(), "P1") {
		t.Errorf("ack row must not carry a priority badge: %q", b.String())
	}
}

// Garbage priorities clamp to medium everywhere — including the badge,
// which must not render "P99".
func TestFormatReadMessage_NoBadgeForGarbagePriority(t *testing.T) {
	loc := time.FixedZone("test", 0)
	m := mkMsg("aa", "foo", "", "hello")
	m.Priority = 99
	var b bytes.Buffer
	FormatReadMessage(&b, m, loc, 0, false)
	if strings.Contains(b.String(), "P99") || strings.Contains(b.String(), "P9") {
		t.Errorf("garbage priority leaked into the badge: %q", b.String())
	}
	if !strings.HasSuffix(b.String(), "  hello\n") {
		t.Errorf("garbage priority should render as plain medium row, got %q", b.String())
	}
}
