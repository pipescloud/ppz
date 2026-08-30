package cliproto

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

// All printers below match the pinned stdout in WIRE.md §8 byte-for-byte.
// Tests diff against expected.txt after normalization — do not change spacing
// or wording without updating WIRE.md and the test fixtures.

// RelativeTime renders the gap from t→now as a coarse human duration:
// "just now" / "N seconds ago" / "N minutes ago" / "N hours ago" / "N days ago".
// Used in `ppz ls` output and the server GUI org page so operators see
// freshness at a glance without parsing absolute timestamps. Negative
// durations (clock skew, future timestamps) clamp to "just now".
func RelativeTime(t, now time.Time) string {
	d := now.Sub(t)
	if d < time.Second {
		return "just now"
	}
	if d < time.Minute {
		n := int(d / time.Second)
		return fmt.Sprintf("%d %s ago", n, pluralUnit(n, "second"))
	}
	if d < time.Hour {
		n := int(d / time.Minute)
		return fmt.Sprintf("%d %s ago", n, pluralUnit(n, "minute"))
	}
	if d < 24*time.Hour {
		n := int(d / time.Hour)
		return fmt.Sprintf("%d %s ago", n, pluralUnit(n, "hour"))
	}
	n := int(d / (24 * time.Hour))
	return fmt.Sprintf("%d %s ago", n, pluralUnit(n, "day"))
}

func pluralUnit(n int, unit string) string {
	if n == 1 {
		return unit
	}
	return unit + "s"
}

func PrintDaemonStarted(w io.Writer, pid int) {
	fmt.Fprintf(w, "daemon started pid=%d\n", pid)
}

func PrintDaemonAlreadyRunning(w io.Writer, pid int) {
	fmt.Fprintf(w, "daemon already running pid=%d\n", pid)
}

func PrintStatus(w io.Writer, s StatusReply) {
	PrintStatusWithEnv(w, s, "", "", false)
}

// statusColors carries ANSI palette closures. When color is false they
// return the input string unchanged so test fixtures and piped output
// stay byte-identical to the no-color case.
type statusColors struct {
	green func(string) string // healthy state, e.g. "logged in", current handle
	red   func(string) string // bad state, e.g. "not running", "authentication error"
	amber func(string) string // soft warning, e.g. "update available" upgrade nudge
	dim   func(string) string // muted, e.g. "-" placeholder for unset current
}

func newStatusColors(enabled bool) statusColors {
	wrap := func(code string) func(string) string {
		if !enabled {
			return func(s string) string { return s }
		}
		return func(s string) string { return "\x1b[" + code + "m" + s + "\x1b[0m" }
	}
	return statusColors{
		green: wrap("32"),
		red:   wrap("31"),
		amber: wrap("33"),
		dim:   wrap("2"),
	}
}

// PrintStatusWithEnv prints `ppz status`. Four states map to the top
// `daemon:` line; the rest of the body depends on which state we're in:
//
//   - not running: just the one line.
//   - not logged in: pid + a hint pointing at `ppz login`.
//   - authentication error: pid + server URL + a hint to refresh the
//     credential. The daemon learned this from a server call returning
//     E_INVALID_API_KEY.
//   - logged in: pid + server URL + org name + current source.
//
// envCurrent / currentJsonPath are the CLI-side facts surfaced for the
// env-vs-daemon disagreement annotation (see source-current-env-precedence
// for the contract). Only consulted when we're in the "logged in" state.
//
// color toggles ANSI colour escapes. The CLI flips it on for interactive
// stdout and off for pipes / NO_COLOR / e2e fixtures.
func PrintStatusWithEnv(w io.Writer, s StatusReply, envCurrent, currentJsonPath string, color bool) {
	PrintStatusWithEnvAndCLIVersion(w, s, envCurrent, currentJsonPath, color, "")
}

func PrintStatusWithEnvAndCLIVersion(w io.Writer, s StatusReply, envCurrent, currentJsonPath string, color bool, cliVersion string) {
	PrintStatusWithUpdateInfo(w, s, envCurrent, currentJsonPath, color, cliVersion, false)
}

// PrintStatusWithUpdateInfo is the canonical `ppz status` printer.
// updateAvailable is the caller's resolved view of "is the CLI behind
// the published manifest" — when true AND daemon == CLI, the daemon
// line renders amber and recommends `ppz upgrade`. Out-of-sync wins
// over update-available (restart first, upgrade after); see status_test
// TestPrintStatus_RedOutOfSyncTakesPriorityOverUpdate.
func PrintStatusWithUpdateInfo(w io.Writer, s StatusReply, envCurrent, currentJsonPath string, color bool, cliVersion string, updateAvailable bool) {
	c := newStatusColors(color)
	if s.DaemonPID == 0 {
		fmt.Fprintf(w, "daemon: %s\n", c.red("not running"))
		return
	}
	if !s.LoggedIn {
		fmt.Fprintf(w, "daemon: %s (pid=%d)%s\n", c.red("not logged in"), s.DaemonPID, daemonVersionSuffix(c, s.DaemonVersion, cliVersion, updateAvailable))
		fmt.Fprintln(w, "hint: run 'ppz login URL -apikey K'")
		return
	}
	if s.LoginCheck == LoginCheckInvalid {
		fmt.Fprintf(w, "daemon: %s (pid=%d)%s\n", c.red("authentication error"), s.DaemonPID, daemonVersionSuffix(c, s.DaemonVersion, cliVersion, updateAvailable))
		fmt.Fprintf(w, "server: %s\n", s.URL)
		fmt.Fprintln(w, "hint: run 'ppz login URL -apikey K' to refresh")
		return
	}
	// LoginCheck is "ok" or "" (probe failed for transient reasons —
	// don't lie, but also don't refuse to render). Treat both as the
	// happy path; the next server-touching call will refresh the cache.
	fmt.Fprintf(w, "daemon: %s (pid=%d)%s\n", c.green("logged in"), s.DaemonPID, daemonVersionSuffix(c, s.DaemonVersion, cliVersion, updateAvailable))
	if s.LastTokenRefreshAt != nil {
		fmt.Fprintf(w, "last token refresh: %s\n", coloredTokenRefreshAge(c, *s.LastTokenRefreshAt, timeNow()))
	} else {
		fmt.Fprintf(w, "last token refresh: %s\n", c.dim("-"))
	}
	fmt.Fprintf(w, "server: %s\n", c.green(s.URL))
	if name := s.AccountName; name != "" {
		fmt.Fprintf(w, "account: %s\n", c.green(name))
	} else {
		fmt.Fprintf(w, "account: %s\n", c.green(s.AccountID))
	}
	fmt.Fprintln(w, formatNATSLine(c, s))

	daemonCurrent := s.Current
	switch {
	case envCurrent != "" && daemonCurrent != "" && envCurrent != daemonCurrent:
		fmt.Fprintf(w, "current source: %s (env PPZ_CURRENT_HANDLE)\n", c.green(envCurrent))
		fmt.Fprintf(w, "current source: %s (pid=%d, %s)\n", c.green(daemonCurrent), s.DaemonPID, currentJsonPath)
		fmt.Fprintln(w, "warning: current source is set twice, env takes precedence")
	case envCurrent != "":
		fmt.Fprintf(w, "current source: %s\n", c.green(envCurrent))
	case daemonCurrent != "":
		fmt.Fprintf(w, "current source: %s\n", c.green(daemonCurrent))
	default:
		fmt.Fprintf(w, "current source: %s\n", c.dim("-"))
	}

	// Phase 1.5: namespace line. Only rendered when set — omitting when
	// empty keeps `ppz status` output tight for the common OSS-default
	// case (no namespace).
	if s.CurrentNamespace != "" {
		fmt.Fprintf(w, "namespace: %s\n", c.green(s.CurrentNamespace))
	}
}

// daemonVersionSuffix renders the trailing `, <version> (<state>)`
// portion of the daemon line. Three states:
//
//   - red "(daemon out of sync with ppz cli, run 'ppz daemon restart')"
//     when the daemon binary's version doesn't match the CLI's. Trumps
//     update-available because the daemon must come back in sync before
//     any upgrade message is meaningful.
//   - amber "(update available, run 'ppz upgrade')" when daemon == CLI
//     AND the caller resolved that a newer release is on the manifest.
//   - green "(latest)" otherwise (daemon == CLI AND no update available
//     OR update check skipped).
func daemonVersionSuffix(c statusColors, daemonVersion, cliVersion string, updateAvailable bool) string {
	if strings.TrimSpace(cliVersion) == "" {
		return ""
	}
	display := strings.TrimSpace(daemonVersion)
	if display == "" {
		display = "version unknown"
	}
	if !versionsMatch(display, cliVersion) {
		return fmt.Sprintf(", %s (daemon out of sync with ppz cli, run 'ppz daemon restart')", c.red(display))
	}
	if updateAvailable {
		return fmt.Sprintf(", %s (update available, run 'ppz upgrade')", c.amber(display))
	}
	return fmt.Sprintf(", %s (latest)", c.green(display))
}

func versionsMatch(a, b string) bool {
	return normaliseVersionForCompare(a) == normaliseVersionForCompare(b)
}

func normaliseVersionForCompare(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	return v
}

// formatNATSLine renders the `nats:` line for `ppz status`. The
// `nats: <state>` prefix is the WIRE.md §8 contract — pinned by
// tests/reliability/nats-status-line. The optional " (N <unit> ago)"
// suffix is a strict extension surfacing connection stability: how
// long has the daemon been in its current NATS state? Drives operator
// intuition for "is this a fresh flap or a stable connection?"
//
// Colour matrix encodes the stability signal:
//
//   - connected + entry=connect  → green (clean first-connect, always stable)
//   - connected + entry=reconnect, age <  60s → amber (just recovered, watch)
//   - connected + entry=reconnect, age >= 60s → green (recovered, holding)
//   - connected + no state-since → green (ring has no anchoring event)
//   - disconnected / closed → red (any age)
//   - connecting → dim
//   - "" (unobserved) → red, label "unknown"
//
// Per-event forensic detail (drop counters, error reasons, full
// timeline) still lives in `ppz diagnostics` — this line is intentionally
// terse.
//
// State "" (daemon hasn't observed a NATS connection yet — fresh
// process pre-login) renders as "unknown" so we don't lie.
func formatNATSLine(c statusColors, s StatusReply) string {
	state := s.NATSState
	if state == "" {
		state = "unknown"
	}
	colored := colorNATSState(c, state, s.NATSStateSince, s.NATSStateEntry)
	suffix := ""
	if s.NATSStateSince != nil && !s.NATSStateSince.IsZero() {
		suffix = " (" + RelativeTime(*s.NATSStateSince, timeNow()) + ")"
	}
	return fmt.Sprintf("nats: %s%s", colored, suffix)
}

// reconnectStableAfter is the minimum age past which a `reconnect` is
// treated as a stable recovery (green) rather than a fresh flap (amber).
// Chosen to comfortably outlast the nats.go reconnect backoff window so
// re-flaps within that window stay visible as amber. Adjust here, not
// at the call site, so the threshold has a single named source.
const reconnectStableAfter = time.Minute

func colorNATSState(c statusColors, state string, since *time.Time, entry string) string {
	switch state {
	case "connected":
		if entry == "reconnect" && since != nil {
			if timeNow().Sub(*since) < reconnectStableAfter {
				return c.amber(state)
			}
		}
		return c.green(state)
	case "disconnected", "unknown":
		return c.red(state)
	case "connecting":
		return c.dim(state)
	}
	return state
}

func coloredTokenRefreshAge(c statusColors, t, now time.Time) string {
	age := now.Sub(t)
	text := RelativeTime(t, now)
	if age >= 5*time.Minute {
		return c.red(text)
	}
	return c.green(text)
}

func PrintLogin(w io.Writer, r LoginReply) {
	fmt.Fprintf(w, "logged in url=%s key=%s account=%s\n", r.URL, r.KeyPrefix, r.AccountID)
}

func PrintCreate(w io.Writer, r CreateReply) {
	fmt.Fprintf(w, "created handle=%s subject=%s\n", r.Handle, r.Subject)
	fmt.Fprintf(w, "current handle=%s\n", r.Handle)
}

func PrintSwitch(w io.Writer, r SwitchReply) {
	fmt.Fprintf(w, "current handle=%s\n", r.Handle)
}

func PrintBroadcast(w io.Writer, r SendReply) {
	fmt.Fprintf(w, "sent id=%s subject=%s bytes=%d\n", r.ID, r.Subject, r.Bytes)
}

func PrintConnect(w io.Writer, r ConnectReply) {
	fmt.Fprintf(w, "connected source=%s\n", r.Handle)
}

func PrintDisconnect(w io.Writer) {
	fmt.Fprintln(w, "disconnected")
}

// PrintPipeCreate prints the pinned line:
//
//	created pipe=<PATH> retention=ttl=<dur>,msgs=<n>,bytes=<b>
//
// PATH is the four-role path with empty slots omitted (Phase 1.5):
//   - Collared root:        cindy.archive
//   - Collared with manifold: team1.cindy.archive
//   - Uncollared root:      room
//   - Uncollared manifold:  team1.room
//
// `dur` is rendered via time.Duration's String() so 24h/168h round-trip
// without manual zero-pad. `bytes` is the raw integer.
func PrintPipeCreate(w io.Writer, r PipeCreateReply) {
	dur := time.Duration(r.TTLSeconds) * time.Second
	fmt.Fprintf(w, "created pipe=%s retention=ttl=%s,msgs=%d,bytes=%d\n",
		FormatPipePath(r.Manifold, r.Handle, r.Name), dur.String(), r.MaxMsgs, r.MaxBytes)
}

// PrintPipeSet prints the pinned line:
//
//	updated pipe=<PATH> retention=ttl=<dur>,msgs=<n>,bytes=<b>
//
// Deliberately the same line as PrintPipeCreate with the verb swapped:
// both commands answer "what does this pipe retain now?", so reading
// either should take the same glance. The retention shown is the fully
// resolved triple, not just the field the user changed.
func PrintPipeSet(w io.Writer, r PipeSetReply) {
	dur := time.Duration(r.TTLSeconds) * time.Second
	fmt.Fprintf(w, "updated pipe=%s retention=ttl=%s,msgs=%d,bytes=%d\n",
		FormatPipePath(r.Manifold, r.Handle, r.Name), dur.String(), r.MaxMsgs, r.MaxBytes)
}

// FormatPipePath renders the four-role pipe path for user display, with
// empty slots omitted. Used by PrintPipeCreate, PrintPipeDestroy, and the
// `to=` field of send output.
func FormatPipePath(manifold, source, name string) string {
	parts := make([]string, 0, 3)
	if manifold != "" {
		parts = append(parts, manifold)
	}
	if source != "" {
		parts = append(parts, source)
	}
	parts = append(parts, name)
	return strings.Join(parts, ".")
}

func PrintPipeDestroy(w io.Writer, r PipeDestroyReply) {
	fmt.Fprintf(w, "destroyed pipe=%s\n", FormatPipePath(r.Manifold, r.Handle, r.Name))
}

func PrintSourceDestroy(w io.Writer, r SourceDestroyReply) {
	path := r.Handle
	if r.Manifold != "" {
		path = r.Manifold + "." + r.Handle
	}
	fmt.Fprintf(w, "destroyed source=%s\n", path)
}

// PrintList prints `ppz ls` output: one line per (source, pipe), sorted
// by handle then pipe name. Format:
//
//	<namespace|->  <handle>.<pipe>  <unread>  <buffered>  <last_at|->  <preview60|->  <creator>
//
// NAMESPACE is the leftmost column — the manifold the pipe lives in,
// rendered as "-" for root or as the dot-separated manifold path
// otherwise. PIPE carries only `<handle>.<pipe>` (or `<pipe>` for
// uncollared) — the manifold prefix moves out of PIPE into NAMESPACE.
//
// UNREAD comes before BUFFERED — agents typically only need the unread
// count to decide whether to call `ppz read`; BUFFERED (the total
// retained in the pipe) is secondary and useful for forensics. CREATOR is
// the rightmost column — the username that owns the (source, pipe).
// Auto-pipes (broadcast / inbox / stdin / stdout / stdctrl) inherit the
// source's CreatedBy; user-created pipes carry their own.
//
// Empty list (no sources) produces no output.
// listRow flattens one (source, pipe) pair into the columns the printers
// align on. iso=true switches the LAST column from relative time to
// RFC3339; the last column otherwise displays "just now" / "5 minutes
// ago". CREATOR is the rightmost column — see PrintList docstring.
//
// namespace carries the row's manifold ('-' for root) and renders as
// the leftmost NAMESPACE column. pipeColumn carries `<handle>.<pipe>`
// for collared rows or `<pipe>` for uncollared rows — the manifold
// prefix lives in namespace, not in pipeColumn, so callers can grep
// the two facts independently.
type listRow struct {
	namespace  string // manifold path, or "-" for root
	pipeColumn string // "<handle>.<pipe>" (collared) or "<pipe>" (uncollared)
	unread     uint64
	buffered   uint64 // total retained messages currently in the stream
	last       string // either RFC3339, relative duration, or "-"
	payload    string // truncated preview (already includes "…" if cut)
	creator    string // username; PipeInfo.CreatedBy ?? Source.CreatedBy
	// Retention cells, rendered only in long mode (`ppz ls -l`) and
	// empty otherwise. Pre-formatted here so writeListTable stays a
	// pure layout function.
	ttl      string
	maxMsgs  string
	maxBytes string
}

// formatTTLColumn renders a TTL for the table. time.Duration's own
// String() gives "24h0m0s" — three columns of noise in a view whose
// point is scanning caps at a glance — so whole hours/minutes collapse
// to "24h" / "5m". 0 means no age limit and renders "-".
func formatTTLColumn(secs int) string {
	if secs <= 0 {
		return "-"
	}
	d := time.Duration(secs) * time.Second
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	}
	return d.String()
}

// formatCapColumn renders a message/byte cap. JetStream spells
// "unlimited" as -1; showing that verbatim reads as a number in a column
// of numbers, so it becomes "∞". Values stay RAW integers otherwise —
// `pipe set --max-bytes` only parses integer mantissas, so a humanised
// "1.4MiB" would print a value that fails when pasted back.
func formatCapColumn(v int64) string {
	if v < 0 {
		return "∞"
	}
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", v)
}

// retentionCells lifts a PipeInfo's retention into its three cells.
func retentionCells(p PipeInfo) (string, string, string) {
	return formatTTLColumn(p.TTLSeconds), formatCapColumn(p.MaxMsgs), formatCapColumn(p.MaxBytes)
}

// namespaceColumn renders a manifold for the NAMESPACE column: empty
// (root) → "-", otherwise the manifold verbatim. Matches the
// missing-value convention used by LAST and PAYLOAD.
func namespaceColumn(manifold string) string {
	if manifold == "" {
		return "-"
	}
	return manifold
}

// PrintList renders sources as an aligned table with a header row. Default
// time format is relative duration ("5 minutes ago" / "just now"); pass
// iso=true for RFC3339 timestamps in the LAST column instead.
//
// CREATOR is rightmost. PAYLOAD becomes a padded column (it used to be
// trailing un-padded, but CREATOR now needs vertical alignment so PAYLOAD
// pads to its widest preview — bounded at 60 chars by TruncatePayload).
func PrintList(w io.Writer, sources []Source, iso bool) {
	PrintListWithUncollared(w, sources, nil, iso, false)
}

// PrintListWithUncollared renders the same table as PrintList but also
// includes uncollared (sourceless) pipes. Phase 1.5.
//
// NAMESPACE owns the manifold for both row shapes; PIPE carries only
// `<handle>.<pipe>` (collared) or `<pipe>` (uncollared) — the manifold
// prefix never appears in PIPE.
// long=true adds the TTL / MAXMSGS / MAXBYTES detail columns, the
// `ls -l` form. They sit immediately after BUFFERED so each cap is
// beside the count it bounds.
func PrintListWithUncollared(w io.Writer, sources []Source, uncollared []UncollaredPipe, iso bool, long bool) {
	now := timeNow()
	rows := make([]listRow, 0)
	for _, s := range sources {
		for _, p := range s.PipeInfos {
			row := listRow{
				namespace:  namespaceColumn(s.Manifold),
				pipeColumn: FormatPipePath("", s.Handle, p.Pipe),
				unread:     p.Unread,
				buffered:   p.Total,
				last:       lastColumn(p.LastAt, now, iso),
				payload:    payloadColumn(p.Preview),
				creator:    humanColumn(p.CreatedBy, s.CreatedBy),
			}
			if long {
				row.ttl, row.maxMsgs, row.maxBytes = retentionCells(p)
			}
			rows = append(rows, row)
		}
	}
	for _, p := range uncollared {
		row := listRow{
			namespace:  namespaceColumn(p.Manifold),
			pipeColumn: FormatPipePath("", "", p.Name),
			unread:     p.Info.Unread,
			buffered:   p.Info.Total,
			last:       lastColumn(p.Info.LastAt, now, iso),
			payload:    payloadColumn(p.Info.Preview),
			creator:    p.Info.CreatedBy,
		}
		if long {
			row.ttl, row.maxMsgs, row.maxBytes = retentionCells(p.Info)
		}
		rows = append(rows, row)
	}
	writeListTable(w, rows, long)
}

// PrintListJSON emits one JSON object per (source, pipe) row in the same
// shape as the API + a `last_at` ISO string. Full untruncated payload —
// `ppz ls` is the only path that surfaces the latest payload without
// going through `ppz read`, so agents reading --json get the real bytes.
//
// `creator` carries the same username the table shows: pipe-level if set,
// otherwise the source's creator (auto-pipe inheritance).
func PrintListJSON(w io.Writer, sources []Source) {
	PrintListJSONWithUncollared(w, sources, nil)
}

// PrintListJSONWithUncollared is the JSON variant including uncollared
// pipes. Phase 1.5. The `namespace` key carries the row's manifold
// (empty string for root) and mirrors the NAMESPACE table column —
// present on every row shape so JSON consumers don't have to special-
// case collared vs uncollared.
// addRetentionJSON adds the retention keys to a `ls --json` row, but
// only when they are populated — which the daemon does only when the
// request set Long. That keeps the default JSON row byte-identical for
// the agents that parse it, without the printer needing to know about
// the flag.
func addRetentionJSON(obj map[string]any, p PipeInfo) {
	if p.TTLSeconds != 0 {
		obj["ttl_seconds"] = p.TTLSeconds
	}
	if p.MaxMsgs != 0 {
		obj["max_msgs"] = p.MaxMsgs
	}
	if p.MaxBytes != 0 {
		obj["max_bytes"] = p.MaxBytes
	}
}

func PrintListJSONWithUncollared(w io.Writer, sources []Source, uncollared []UncollaredPipe) {
	for _, s := range sources {
		for _, p := range s.PipeInfos {
			obj := map[string]any{
				"namespace": s.Manifold,
				"handle":    s.Handle,
				"pipe":      p.Pipe,
				"total":     p.Total,
				"unread":    p.Unread,
				"payload":   p.Payload,
				"creator":   humanColumn(p.CreatedBy, s.CreatedBy),
			}
			addRetentionJSON(obj, p)
			if p.LastAt != nil {
				obj["last_at"] = p.LastAt.UTC().Format(time.RFC3339)
			} else {
				obj["last_at"] = nil
			}
			line, _ := json.Marshal(obj)
			fmt.Fprintln(w, string(line))
		}
	}
	for _, p := range uncollared {
		obj := map[string]any{
			"namespace": p.Manifold,
			"handle":    "",
			"pipe":      p.Name,
			"total":     p.Info.Total,
			"unread":    p.Info.Unread,
			"payload":   p.Info.Payload,
			"creator":   p.Info.CreatedBy,
		}
		addRetentionJSON(obj, p.Info)
		if p.Info.LastAt != nil {
			obj["last_at"] = p.Info.LastAt.UTC().Format(time.RFC3339)
		} else {
			obj["last_at"] = nil
		}
		line, _ := json.Marshal(obj)
		fmt.Fprintln(w, string(line))
	}
}

// IsGlobPattern reports whether a subscription subject is a glob/pattern
// (expands at read-time) rather than a concrete subject. Covers
// filepath.Match metacharacters (`*`, `?`, `[`) plus `%`, the SQL-LIKE
// alias the daemon's matchAnyTarget rewrites to `*`. Shared by the daemon
// (snapshot) and the CLI (tree render) so the two agree on what a pattern is.
func IsGlobPattern(s string) bool {
	return strings.ContainsAny(s, "*?[%")
}

// subsRow is one rendered line of the `subs ls` tree. A pattern PARENT and a
// pipe row both fill all columns; a note row (e.g. the `(no matches)` leaf)
// occupies only NAMESPACE+PIPE so it renders clean with no trailing dashes.
type subsRow struct {
	namespace string
	pipe      string // includes the `├─ ` / `└─ ` tree glyph for child rows
	unread    string
	buffered  string
	last      string
	payload   string
	creator   string
	note      bool
}

// PrintSubsList renders `ppz subs ls` as a tree. A glob/pattern subscription
// (e.g. `test-%`) is a PARENT row; the pipes it currently matches render as
// indented children beneath it, so attribution is visible — you can see which
// pattern surfaced which pipe. A pattern matching nothing still shows, with a
// `(no matches)` leaf, rather than vanishing. A literal subscription (a
// concrete `<handle>.<pipe>` or uncollared pipe) renders as a plain top-level
// row, exactly as `ppz ls` would.
//
// Columns match `ppz ls` (NAMESPACE … CREATOR) so users don't learn two
// formats; a parent/pattern row carries `-` in the stat columns since it is a
// subscription, not a pipe.
//
// Overlap (a pipe matched by more than one subscription) is rendered once,
// under the first subscription in sorted order that matched it; the full
// match set is always available in `--json`'s matched_by. Subscriptions
// arrive pre-sorted, which also fixes the top-level render order.
func PrintSubsList(w io.Writer, sources []Source, uncollared []UncollaredPipe, subscriptions []string, iso bool) {
	now := timeNow()

	// Flatten every surfaced pipe into a row keyed by its full target, with
	// the subscriptions that matched it carried alongside for grouping.
	type pipeRow struct {
		target    string
		matchedBy []string
		cells     subsRow
		claimed   bool
	}
	var rows []*pipeRow
	add := func(target, ns, pipeCol string, info PipeInfo, sourceCreatedBy string) {
		rows = append(rows, &pipeRow{
			target:    target,
			matchedBy: info.MatchedBy,
			cells: subsRow{
				namespace: ns,
				pipe:      pipeCol,
				unread:    fmt.Sprintf("%d", info.Unread),
				buffered:  fmt.Sprintf("%d", info.Total),
				last:      lastColumn(info.LastAt, now, iso),
				payload:   payloadColumn(info.Preview),
				creator:   humanColumn(info.CreatedBy, sourceCreatedBy),
			},
		})
	}
	for _, s := range sources {
		for _, p := range s.PipeInfos {
			add(s.Handle+"."+p.Pipe, namespaceColumn(s.Manifold), FormatPipePath("", s.Handle, p.Pipe), p, s.CreatedBy)
		}
	}
	for _, p := range uncollared {
		add(FormatPipePath(p.Manifold, "", p.Name), namespaceColumn(p.Manifold), FormatPipePath("", "", p.Name), p.Info, "")
	}
	byTarget := make(map[string]*pipeRow, len(rows))
	for _, r := range rows {
		byTarget[r.target] = r
	}

	var out []subsRow
	for _, subj := range subscriptions {
		if IsGlobPattern(subj) {
			out = append(out, subsRow{namespace: "-", pipe: subj, unread: "-", buffered: "-", last: "-", payload: "-", creator: "-"})
			var kids []*pipeRow
			for _, r := range rows {
				if !r.claimed && containsString(r.matchedBy, subj) {
					kids = append(kids, r)
				}
			}
			sort.Slice(kids, func(i, j int) bool { return kids[i].target < kids[j].target })
			if len(kids) == 0 {
				out = append(out, subsRow{namespace: "-", pipe: "└─ (no matches)", note: true})
				continue
			}
			for i, k := range kids {
				k.claimed = true
				glyph := "├─ "
				if i == len(kids)-1 {
					glyph = "└─ "
				}
				row := k.cells
				row.pipe = glyph + k.cells.pipe
				out = append(out, row)
			}
			continue
		}
		// Literal subscription → its own concrete row. subsSnapshot always
		// emits a row (real or synthetic zero-row) for a literal, so this is
		// present unless an earlier pattern already claimed the same pipe.
		if r := byTarget[subj]; r != nil && !r.claimed {
			r.claimed = true
			out = append(out, r.cells)
		}
	}
	writeSubsTable(w, out)
}

// PrintSubsListJSON is the `subs ls --json` variant: FLAT, one object per
// matched pipe (same base shape as `ls --watch --json`), each row carrying
// `matched_by` — the subscription(s) that surfaced it. The tree is a
// human-presentation concern only; JSON consumers get attribution as a field.
func PrintSubsListJSON(w io.Writer, sources []Source, uncollared []UncollaredPipe) {
	emit := func(namespace, handle, pipe string, info PipeInfo, creator string) {
		obj := map[string]any{
			"namespace":  namespace,
			"handle":     handle,
			"pipe":       pipe,
			"total":      info.Total,
			"unread":     info.Unread,
			"payload":    info.Payload,
			"creator":    creator,
			"matched_by": info.MatchedBy,
		}
		if info.LastAt != nil {
			obj["last_at"] = info.LastAt.UTC().Format(time.RFC3339)
		} else {
			obj["last_at"] = nil
		}
		line, _ := json.Marshal(obj)
		fmt.Fprintln(w, string(line))
	}
	for _, s := range sources {
		for _, p := range s.PipeInfos {
			emit(s.Manifold, s.Handle, p.Pipe, p, humanColumn(p.CreatedBy, s.CreatedBy))
		}
	}
	for _, p := range uncollared {
		emit(p.Manifold, "", p.Name, p.Info, p.Info.CreatedBy)
	}
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// writeSubsTable prints the `subs ls` tree with the same 7 columns as
// `ppz ls`. Kept separate from writeListTable because subs rows need string
// cells (a parent/pattern row shows `-` not `0` in UNREAD/BUFFERED, and child
// rows carry a tree glyph) that the numeric, byte-pinned ls table can't
// express. Column widths are sized to content; the elastic-PAYLOAD / LAST
// anti-drift tuning of `ppz ls` is intentionally omitted — the subs view is
// small and its output is whitespace-normalized in tests.
func writeSubsTable(w io.Writer, rows []subsRow) {
	if len(rows) == 0 {
		return
	}
	headers := []string{"NAMESPACE", "PIPE", "UNREAD", "BUFFERED", "LAST", "PAYLOAD", "CREATOR"}
	widths := []int{len(headers[0]), len(headers[1]), len(headers[2]), len(headers[3]), len(headers[4]), len(headers[5])}
	// CREATOR is the trailing column, printed with a bare %s, so it needs no
	// width tracking (nothing follows it to align against).
	grow := func(i, l int) {
		if l > widths[i] {
			widths[i] = l
		}
	}
	for _, r := range rows {
		grow(0, dispWidth(r.namespace))
		grow(1, dispWidth(r.pipe))
		if r.note {
			continue
		}
		grow(2, dispWidth(r.unread))
		grow(3, dispWidth(r.buffered))
		grow(4, dispWidth(r.last))
		grow(5, dispWidth(r.payload))
	}
	fmt.Fprintf(w, "%s  %s  %s  %s  %s  %s  %s\n",
		padRightDisp(headers[0], widths[0]), padRightDisp(headers[1], widths[1]), padRightDisp(headers[2], widths[2]),
		padRightDisp(headers[3], widths[3]), padRightDisp(headers[4], widths[4]), padRightDisp(headers[5], widths[5]), headers[6])
	for _, r := range rows {
		if r.note {
			fmt.Fprintf(w, "%s  %s\n", padRightDisp(r.namespace, widths[0]), r.pipe)
			continue
		}
		fmt.Fprintf(w, "%s  %s  %s  %s  %s  %s  %s\n",
			padRightDisp(r.namespace, widths[0]), padRightDisp(r.pipe, widths[1]), padRightDisp(r.unread, widths[2]),
			padRightDisp(r.buffered, widths[3]), padRightDisp(r.last, widths[4]), padRightDisp(r.payload, widths[5]), r.creator)
	}
}

// humanColumn implements the auto-pipe inheritance rule: a pipe carries
// its own creator when one is set (user-created `pipes` row), otherwise
// it inherits the source's creator (auto-provisioned pipes have no row
// in the `pipes` table, so PipeInfo.CreatedBy is empty).
func humanColumn(pipeCreatedBy, sourceCreatedBy string) string {
	if pipeCreatedBy != "" {
		return pipeCreatedBy
	}
	return sourceCreatedBy
}

func lastColumn(t *time.Time, now time.Time, iso bool) string {
	if t == nil {
		return "-"
	}
	if iso {
		return t.UTC().Format(time.RFC3339)
	}
	return RelativeTime(*t, now)
}

func payloadColumn(preview string) string {
	if preview == "" {
		return "-"
	}
	return preview
}

// dispWidth reports the number of terminal columns s occupies. Unlike
// len() (bytes) or utf8.RuneCountInString (runes), it accounts for wide
// runes (CJK, emoji — 2 columns) and zero-width combining marks (0). All
// table column sizing/padding goes through this so an embedded icon (e.g.
// 📊 in a status-report payload) doesn't drift the columns to its right.
func dispWidth(s string) int {
	return runewidth.StringWidth(s)
}

// padRightDisp left-justifies s in a field of width display columns,
// padding the right with spaces. fmt's "%-*s" pads by rune count, which
// mis-sizes wide and zero-width runes — so every table cell is padded
// here, by display width, instead of via the format verb.
func padRightDisp(s string, width int) string {
	if pad := width - dispWidth(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// truncateForColumn cuts s so it occupies at most maxCols terminal
// columns, replacing the trailing content with "…" when truncation
// actually occurred. Operates on display width (not bytes or runes) so
// wide runes are budgeted correctly and a multi-byte rune is never sliced
// mid-codepoint. Used by writeListTable to fit the PAYLOAD column into the
// leftover terminal-width budget.
func truncateForColumn(s string, maxCols int) string {
	if maxCols <= 0 {
		return ""
	}
	if dispWidth(s) <= maxCols {
		return s
	}
	if maxCols == 1 {
		return "…"
	}
	// Reserve one column for the trailing "…" (itself 1 column wide).
	limit := maxCols - 1
	w := 0
	var b strings.Builder
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > limit {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

// writeListTable computes max widths for every column (including PAYLOAD,
// which used to be the trailing un-padded column) and prints header +
// rows aligned. CREATOR is the rightmost column — it goes un-padded since
// nothing follows it. NAMESPACE is the leftmost column; its width grows
// to the widest manifold path among the rows.
//
// Empty input → empty output (no orphan header). Matches the convention
// where `ls` for an empty namespace just prints nothing.
// writeListTable lays out the rows. Column set is data-driven rather
// than hardcoded so the long form's three extra columns reuse the same
// width budgeting: every column sizes to its widest cell, LAST carries an
// anti-drift minimum, PAYLOAD is the elastic one that absorbs whatever
// the terminal has left, and CREATOR is rightmost and un-padded.
func writeListTable(w io.Writer, rows []listRow, long bool) {
	if len(rows) == 0 {
		return
	}
	headers := []string{"NAMESPACE", "PIPE", "UNREAD", "BUFFERED"}
	if long {
		headers = append(headers, "TTL", "MAXMSGS", "MAXBYTES")
	}
	headers = append(headers, "LAST", "PAYLOAD", "CREATOR")

	cells := make([][]string, len(rows))
	for i, r := range rows {
		c := []string{r.namespace, r.pipeColumn, fmt.Sprintf("%d", r.unread), fmt.Sprintf("%d", r.buffered)}
		if long {
			c = append(c, r.ttl, r.maxMsgs, r.maxBytes)
		}
		cells[i] = append(c, r.last, r.payload, r.creator)
	}

	// The three trailing columns are positional, not fixed indices: the
	// long form shifts them right by three.
	n := len(headers)
	lastIdx, payloadIdx, creatorIdx := n-3, n-2, n-1

	widths := make([]int, n)
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, c := range cells {
		for i, v := range c {
			if dw := dispWidth(v); dw > widths[i] {
				widths[i] = dw
			}
		}
	}

	const sep = 2
	separators := sep * (n - 1)
	// totalExcept sums every column width but one, substituting `sub`
	// for the excluded column.
	totalExcept := func(idx, sub int) int {
		total := separators + sub
		for i, wd := range widths {
			if i != idx {
				total += wd
			}
		}
		return total
	}

	// Pin a minimum LAST width so common relative-time rollovers
	// ("9 minutes ago" → "10 minutes ago", "1 hour ago" → "2 hours ago")
	// don't visibly drift PAYLOAD/CREATOR rightward between successive
	// `ppz ls` invocations. 14 covers most steady-state values:
	// "XX minutes ago" = 14, "X days ago" = 10, "just now" = 8.
	// Rare wider values ("59 minutes ago" = 15) can still push past it.
	//
	// Skip the pin when the terminal is too narrow to absorb the extra
	// width — anti-drift is a "nice to have" that shouldn't push rows
	// off-screen on small windows.
	const lastMinWidth = 14
	if widths[lastIdx] < lastMinWidth && totalExcept(lastIdx, lastMinWidth) <= TerminalWidth() {
		widths[lastIdx] = lastMinWidth
	}

	// Cap the PAYLOAD column to fit the caller's terminal width. The
	// other columns are sized to their data — payload is the elastic
	// one. Anything left over after the fixed-width columns + separators
	// becomes the payload budget. If the budget is negative (very narrow
	// terminal vs wide handles), leave payload at its natural width —
	// the row will overflow rather than corrupting alignment of the
	// inner columns.
	if budget := TerminalWidth() - totalExcept(payloadIdx, 0); budget > 0 && budget < widths[payloadIdx] {
		widths[payloadIdx] = budget
		for i := range cells {
			cells[i][payloadIdx] = truncateForColumn(cells[i][payloadIdx], budget)
		}
	}

	writeRow := func(cells []string) {
		for i, v := range cells {
			if i == creatorIdx {
				fmt.Fprintf(w, "%s\n", v)
				continue
			}
			fmt.Fprintf(w, "%s  ", padRightDisp(v, widths[i]))
		}
	}
	writeRow(headers)
	for _, c := range cells {
		writeRow(c)
	}
}

// timeNow is overridable in tests if we ever need deterministic relative
// times. Production: just time.Now.
var timeNow = func() time.Time { return time.Now() }

// TruncatePayload renders a payload for display in `ppz ls`: strip ANSI
// CSI escapes (ESC `[` … final-byte) and all C0 controls + DEL so the
// preview can never alter the user's terminal state, replace newlines with
// spaces, trim trailing whitespace, then cap at 60 bytes (UTF-8 safe).
//
// Without this, a wrapped terminal's last .stdout chunk — typically a
// shell prompt with cursor moves and colour escapes — would render
// verbatim mid-listing and clear the screen, set bold, etc.
func TruncatePayload(s string) string {
	s = stripANSI(s)
	s = stripControlBytes(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimRight(s, " \t")
	if len(s) > 60 {
		// Cap at 60 bytes but don't slice mid-rune. The trailing "…"
		// signals to humans that the payload was cut — full text is
		// available via `ppz read` or `ppz ls --json`.
		end := 60
		for end > 0 && (s[end-1]&0xC0) == 0x80 {
			end--
		}
		s = s[:end] + "…"
	}
	return s
}

// stripANSI removes CSI escape sequences (ESC `[` <params> <final-byte>),
// where final-byte is in the range 0x40-0x7E. Other ESC sequences (single-
// shot, OSC strings) aren't fully parsed — we just drop the bare ESC byte
// in stripControlBytes, which covers the common shell-prompt case without
// implementing a full terminal emulator.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == 0x1B && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3F {
				j++
			}
			if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7E {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// stripControlBytes drops C0 controls (< 0x20) and DEL (0x7F), preserving
// only \n / \r / \t — the caller normalises those separately.
func stripControlBytes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\r' || c == '\t' || (c >= 0x20 && c != 0x7F) {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// FprintError writes the standard one-line error to stderr.
func FprintError(w io.Writer, err *Error) {
	fmt.Fprintf(w, "error: %s: %s\n", err.Code, err.Message)
}
