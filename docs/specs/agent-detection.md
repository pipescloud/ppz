# Spec: PTY agent-harness detection (auto harness/state in heartbeats, `ppz who` combined status)

## Context

Heartbeats already carry `harness` and `model` fields, but they are stamped from
`PPZ_AGENT_HARNESS` / `PPZ_AGENT_MODEL` env vars that only `ppz agent` sets
(`internal/cli/agent.go:80`). A user who runs `ppz terminal share` and then starts
`claude` by hand shows up in `ppz who` as a bare shell; conversely the env vars stay
set after the harness exits back to a shell. The wrapper also has harness-conditional
behavior (alert submit keys, `submitAlertToPTY` in `terminal_subs_alert.go`) that silently degrades for
manual launches.

herdr solves this for its panes with three layered mechanisms; this epic ports the
applicable parts into ppz's wrapper and heartbeat pipeline, and surfaces the result in
`ppz who` as a combined status column (`online|working`, `online|blocked`, …).

## Phases

| Phase | What | Mechanism | Status |
|-------|------|-----------|--------|
| 1 | Detect *which* harness is running in the wrapped PTY | foreground process-group inspection | implemented (2026-06-10) |
| 2 | Detect working/idle + combined `ppz who` status | PTY byte causality | implemented (2026-06-10) |
| 3 | Detect blocked (permission prompts) | live vt10x screen model + per-harness patterns | implemented, Claude-first (2026-06-11) |
| 4 | Detect model without env vars | best-effort, Claude-first (transcript tail) | future |

Phases 1+2 shipped together: identification feeds the heartbeat, causality feeds
the state, `ppz who` renders both. Unit + wrapper wiring are in (including a
real-PTY integration test for the TIOCGPGRP inspector seam), as is the e2e
fixture (`share-heartbeat-detects-harness`).

---

## Prior art: herdr

(Repo: `~/external-source/herdr`. Surveyed 2026-06-10.)

- **Identification** is process-tree based, not output based: it reads the PTY's
  foreground process group, normalizes the binary name (strip `.exe`/`.js`/…,
  lowercase), and falls back to scanning argv when the leader is a generic runtime
  (`node -e`, `python -c`, `bash -c`) — `src/detect/mod.rs:114-167`. Rechecks every
  0.5–2s during an 8s acquisition window, then 5s.
- **Working vs idle** is byte-causality based: PTY output keeps an "active" window
  alive for **1800ms**; user keystrokes "taint" causality for **1200ms** so echoes
  aren't mistaken for agent work; a **3s startup grace** suppresses boot noise
  (`src/pane/agent_detection.rs`). Screen-content regexes only arbitrate idle vs
  blocked — PTY activity is the authority for working.
- **Blocked** requires the parsed screen (bottom ~50 lines via a VT emulator) and
  per-agent pattern modules (`src/detect/agents/*.rs`, 17 agents).
- **Extensibility model**: one module per agent implementing
  `detect(content) -> AgentState` (+ optional `has_visible_blocker`), and a central
  dispatch enum (`src/detect/agents/mod.rs:21`). Adding an agent = enum variant +
  binary-name rows + one pattern module.
- **Model detection**: herdr deliberately has none ("detection is decoupled",
  AGENTS.md) — a meaningful signal about its reliability.

**Licensing boundary**: herdr is AGPL-3.0-or-later and third-party
(github.com/ogulcancelik/herdr); ppz is Apache-2.0. herdr is used as a
behavioral reference only — architecture ideas, timing parameters, and factual
UI pattern strings (which are facts about the agents' UIs, not herdr's
expression). No herdr source is copied or closely translated, including test
code. A function-for-function port would make ppz a derivative work and pull
the AGPL across the project.

The Go translation of herdr's extensibility model: a data table of `Spec` rows for
identification (phase 1), and a per-harness `ScreenDetector` interface registered on
the row for phase 3. Adding a harness is one row now, one row + one small file later.

---

## Design

### New package: `internal/harness`

All detection logic lives here, pure and platform-free, so the wrapper just wires it
up and unit tests need no PTY.

```go
// State of a wrapped agent harness, as stamped into heartbeats.
type State string
const (
    StateUnknown State = ""        // no harness identified
    StateIdle    State = "idle"
    StateWorking State = "working"
    StateBlocked State = "blocked" // phase 3
)

// Spec describes one known harness. Extending detection to a new
// harness is one new row. Names match `ppz agent`'s --harness ids.
type Spec struct {
    Name        string   // "claude", "codex", "copilot", "agy", "pi"
    BinaryNames []string // e.g. {"claude", "claude-code"}
}
func Specs() []Spec

// Identify maps a foreground process to a canonical harness name ("" =
// not a harness). Handles name normalization (case, .exe/.js/… suffixes)
// and wrapped runtimes (node/python/sh argv scanning), per herdr.
func Identify(comm string, argv []string) string

// ActivityTracker classifies working/idle from byte causality.
// Constants ported from herdr: 1800ms output-activity window, 1200ms
// input-taint window, 3s startup grace.
func NewActivityTracker(start time.Time) *ActivityTracker
func (t *ActivityTracker) ObserveOutput(now time.Time)
func (t *ActivityTracker) ObserveInput(now time.Time)
func (t *ActivityTracker) State(now time.Time) State

// Detector composes identification + activity for one wrapped PTY.
// Inspect is the platform seam (TIOCGPGRP + process lookup in prod,
// a fake in tests).
type ForegroundProc struct{ PID int; Comm string; Argv []string }
type Detection struct{ Harness string; ChildPID int; State State }
func NewDetector(inspect func() (ForegroundProc, error)) *Detector
// (activity tracking is keyed to identification time — the Poll that
// first sees a harness — so the detector takes no clock input)
func (d *Detector) Poll(now time.Time)
func (d *Detector) ObserveOutput/ObserveInput(now time.Time)
func (d *Detector) Snapshot(now time.Time) Detection
```

Detector behavior decisions:
- Inspect **error retains** the previous identification (a transient `ps` failure
  must not flap the column); a successful inspect of a non-harness **clears** it.
- Harness change (including re-launch) resets the activity tracker, so each
  identification gets its own startup grace.
- Phase 2 only emits working/idle; blocked arrives in phase 3 via an optional
  per-harness `ScreenDetector` consulted when causality says "not working".

### Wrapper integration (`internal/cli/terminal.go`)

- **Inspector**: foreground pgid via `TIOCGPGRP` ioctl on the PTY master, then
  comm/argv via `/proc/<pid>/{comm,cmdline}` on linux, `ps -o comm=,args= -p` on
  darwin. Poll 1s for the first 10s after spawn (herdr's acquisition window,
  simplified), then 5s.
- **ObserveOutput**: tee in `publishAndDisplayStdout()` (terminal.go:486) — zero
  extra syscalls, just a timestamp store.
- **ObserveInput**: local stdin copy loop, remote `<handle>.stdin` forwarding
  (`forwardStdin`), and alert auto-submission (`submitAlertToPTY`) all count as
  input taint.

### Heartbeat wire changes (`internal/cli/heartbeat.go`)

Three new fields, always-serialized per repo convention (predictable wire shape,
see request-ack.md):

```go
HarnessSource string `json:"harness_source"` // "detected" | "env" | ""
ChildPID      int    `json:"child_pid"`      // foreground pid when detected
AgentState    string `json:"agent_state"`    // "" | "idle" | "working" | "blocked"
```

- **Precedence**: detection wins over env. `resolveHarness(detected, env)`:
  detected non-empty → (detected, "detected"); else env non-empty → (env, "env");
  else ("", ""). Rationale: env is launch-time and survives the harness exiting;
  detection tracks the live foreground.
- `model` is unchanged in phases 1–2 (env-only until phase 4).
- **Freshness**: 60s beats are too coarse for state. `heartbeatDeps` gains a
  `StateChanged <-chan struct{}` wake channel — the wrapper signals on working/idle
  transitions and the loop emits an immediate out-of-cycle beat (seq still
  monotonic). No debounce needed in v1: the tracker's hysteresis already damps
  transitions to human timescales; the channel send is non-blocking (buffered 1) so
  bursts coalesce.
- Compat: additive JSON; old daemons/CLIs ignore unknown keys, new CLIs render `-`
  for missing keys. No version bump. WIRE.md heartbeat section gets the new keys.

### `ppz who` combined status

New single-source-of-truth rule next to `ClassifyHeartbeatStatus`
(`internal/daemon/heartbeat_status.go`):

```go
// CombineHeartbeatStatus merges liveness with agent state:
//   offline, *        → "offline"           (state too old to be meaningful)
//   online, ""        → "online"            (plain shell / no harness)
//   online, working   → "online|working"
//   stale,  working   → "stale|working"     (amber colour conveys doubt)
func CombineHeartbeatStatus(liveness string, agentState string) string
```

- Table STATUS column shows the combined string; the ANSI colour stays keyed to
  liveness (green/amber/red) wrapping the whole cell. (A distinct colour for
  `blocked` is a phase 3 candidate.)
- `--json` keeps machine-readable fields separate: top-level `status` stays
  liveness-only; consumers read `heartbeat.agent_state` and compose. The table is
  for humans, the JSON for scripts.
- Existing `--online/--stale/--offline` filters unchanged (liveness-based). A
  `--state working|idle|blocked` filter is a follow-on.

---

## Phase 3 (blocked) — implemented 2026-06-11, Claude-first

The wrapper keeps a private live instance of the bundled vt10x emulator
(`cliproto.LiveScreen`): the output tee feeds it every byte the child draws, the
winsize paths mirror SIGWINCH into it, and the detector consults its bottom 50
lines (`harnessScreenBottomLines`) — but only when byte causality already says
"not working". PTY activity stays the authority for working; the screen splits
idle into idle vs blocked. Blocked is startup-grace-exempt: grace suppresses
false *working* from boot noise, but a permission prompt at boot (resume onto a
dialog) is a real question.

Per-harness patterns hang off `harness.ScreenDetector` (`ScreenDetectorFor`);
Claude Code (`screen_claude.go`) recognizes permission/edit dialogs,
selector-chrome forms, permission waits, and interview screens, with two
false-positive guards: working chrome ("esc to interrupt") vetoes stale dialog
text, and a live input prompt box (a ❯ line that isn't a numbered selector)
marks question text above it as history/ghost cells rather than a live dialog.
Adding a harness = one `ScreenDetectorFor` case + one `screen_<name>.go`.

Pre-work outcomes:

1. Incremental writes validated: `TestLiveScreen_ChunkedFeedMatchesOneShot`
   replays the real session fixtures at chunk sizes 1/7/4096 (1-byte chunks
   split every escape sequence and rune) and matches one-shot `RenderTerminal`
   exactly — vt10x's parser state survives arbitrary chunking; only UTF-8
   tearing needed handling (LiveScreen carries trailing partial runes).
2. The vt10x stale-cell bug is *mitigated, not fixed*: the live-prompt-box and
   working-chrome vetoes mean ghost dialog text can't flip an interactive
   session to blocked on its own. The bug's pinned render test remains; fixing
   the emulator is still worthwhile independent hardening.

Follow-on hardening: ground-truth fixtures captured from real blocked Claude
sessions (`ppz read <h>.stdout --raw` while a dialog is up) to back the
synthetic pattern tests; codex/copilot/agy/pi pattern modules.

## Phase 4 sketch (model) — future, best-effort

Env var stays the reliable path for `ppz agent` launches. For detected Claude
sessions: tail the newest transcript JSONL under `~/.claude/projects/<cwd-slug>/`
modified since child start (messages carry the model id). Explicitly best-effort;
herdr skipped this entirely. Other harnesses: on-screen scrape via the phase 3
screen model, version-fragile, last.

---

## Test plan (RED first — this PR)

Unit, `internal/harness` (new):
- `identify_test.go`: direct names for all five harnesses incl. aliases
  (`claude-code`, `github-copilot`/`ghcs`, `antigravity`); normalization (case,
  `.exe`, `.js`, whitespace); wrapped runtimes (`node <path>/claude`,
  `bash -c "claude --resume"`); negatives (`vim`, `tmux`, bare `node`, bare shell,
  empty comm).
- `activity_test.go`: fresh→idle; untainted output→working; window expiry→idle;
  echo during taint stays idle; output after taint expiry→working; output during
  startup grace doesn't count; sustained typing+echo never working.
- `detector_test.go`: identifies foreground harness (pid, state idle); clears when
  harness exits to shell; retains identification on inspect error; output marks
  working; re-identification resets grace.

Unit, `internal/cli`:
- `heartbeat_agent_test.go`: `resolveHarness` precedence matrix; builder maps
  `HarnessSource`/`ChildPID`/`AgentState` inputs→payload; `runHeartbeat` stamps a
  detection snapshot (detected wins, env fallback when empty); `StateChanged` wake
  emits an immediate beat without a tick.
- `heartbeat_test.go`: JSON-shape key set extended (`agent_state`, `child_pid`,
  `harness_source`).
- `who_state_test.go`: table renders `online|working` / `online|idle` /
  `online|blocked` / `stale|working`; offline rows show plain `offline`; colour
  mode wraps the combined cell in the liveness colour.

Unit, `internal/daemon`:
- `heartbeat_status_combined_test.go`: `CombineHeartbeatStatus` matrix.

E2e (follow-on, after GREEN): `tests/terminal/share-heartbeat-detects-harness` —
stub `claude` script on PATH inside a shared terminal; assert `ppz who` shows
`claude` + `online|working` while it streams output, `online|idle` after. Run
targeted via `PPZ_TEST_FILTER`; not part of the RED commit (unit seams cover the
logic; e2e pins the wiring).

## Out of scope / follow-ons

- Blocked state, per-harness screen patterns, vt10x hardening (phase 3).
- Model detection (phase 4).
- Alert submit-key selection still keys off the env-var harness
  (`terminalSubsAlertConfig.Harness`, terminal.go): a hand-launched claude
  gets the `\r` fallback instead of kitty Enter. Switching it to live
  detection needs the pump to re-read the harness at fire time, not
  construction time.
- `ppz who --state <state>` filter; distinct colour for blocked.
- A dedicated `.agentstate` event channel (immediate-beat-on-transition covers
  `ppz who` freshness; revisit if a push consumer appears).
- Windows (`ConPTY` has no foreground pgrp; wrapper currently unix-only anyway).

## Decisions taken (flag in review if disagreed)

1. Detection wins over env (`harness_source` records which).
2. `stale` rows keep the state suffix (`stale|working`); `offline` drops it.
3. `--json` `status` stays liveness-only; combined string is table-only.
4. herdr timing constants adopted verbatim (1800/1200ms, 3s grace).
5. State-transition beats are unthrottled beyond tracker hysteresis.
