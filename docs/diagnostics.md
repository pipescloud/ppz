# NATS Diagnostics — Reference & Runbook

This document is the reading order for anyone — human or AI agent —
debugging a `E_NATS_UNREACHABLE`, a NATS-connection blip, or any other
symptom that surfaces through `ppz diagnostics`.

It is the contract between the daemon (which emits events) and any
reader of those events (CLI, bundle inspector, future analyses). Code
references in this file point into `internal/daemon/nats_events.go`
and friends.

---

## 1. Quick start

```sh
# What's happening right now (default — pit-of-success output)
ppz diagnostics

# Same, but over the last hour from on-disk history
ppz diagnostics --since=1h

# Capture everything into a tarball for a bug report
ppz diagnostics --bundle
# → prints path to ~/ppz-diag-<timestamp>.tgz

# Machine-readable; patterns are first-class in the JSON
ppz diagnostics --json
```

The error message for `E_NATS_UNREACHABLE` names `--bundle` so users
discover it without reading this file. Keep that pointer alive in
`internal/cliproto/errors.go` — the test
`TestMessage_ENATSUnreachable_MentionsBundle` enforces it.

---

## 2. Reading the default output

```
nats: connected  (since 00:34:02, 4m23s ago)  drops_last_hour=2
refresh: last 00:34:05 (4m20s ago), next due in 35s
url: nats://pipescloud.io:4222

⚠ burst-swap-storm  00:33:30
   12 swaps in 1.2s — concurrent NC mutation (callers: …); see docs/diagnostics.md#burst-swap-storm

Recent events (14 shown, scope=ring, 247 older on disk):
  00:34:02  reconnect  caller=ensureNATS                nc=0x140004f3  reason="rebuilt"
  ...

→ Full history:  ppz diagnostics --since=1h
→ Bug report:    ppz diagnostics --bundle  (writes ~/ppz-diag-<ts>.tgz)
```

Order matters:

1. **Summary** — current state, refresh timing, broker URL. Read first.
   "since 4m23s ago" means the connection has been stable for ~4m.
   `drops_last_hour=2` is a quick health metric. It counts only
   unplanned disconnects: teardowns caused by the daemon's own
   refresh-rotation swaps (a `swap` naming the conn as `old=` just
   before) are excluded, so a healthy hour reads 0 — not ~14.
2. **Pattern warnings** — auto-detected anomalies. If this block is
   empty, the recent trace is clean. Each `⚠` line names the pattern
   (anchors into §6), the wall-clock time, and a one-line interpretation.
3. **Recent events** — chronological tail, one transition per line.
   Columns: time, type, caller, nc=ID, reason. The `scope=` tag tells
   you whether you're looking at the in-memory ring (last few minutes)
   or a `--since` scan of the on-disk log.
4. **Hint footer** — always shown. Tells you how to widen the window
   or capture a bundle.

---

## 3. Schema (v1)

The on-disk jsonl file and the IPC `DiagReply` share `NATSEvent`'s
schema. Every line/object carries `"v": 1`. Bump `NATSEventSchemaVersion`
in `nats_events.go` when fields are renamed or semantics change; new
fields with `json:",omitempty"` do NOT require a bump.

### NATSEvent

| Field     | Type   | Required | Meaning                                                                   |
|-----------|--------|----------|---------------------------------------------------------------------------|
| `v`       | int    | yes      | Schema version; currently `1`.                                            |
| `type`    | string | yes      | One of the closed set in §4.                                              |
| `at`      | RFC3339| yes      | UTC timestamp when the event was recorded.                                |
| `caller`  | string | no       | Function name that initiated the transition (§5). `"nats.go"` for library callbacks. |
| `nc_id`   | string | no       | Pointer-identity of the `*nats.Conn` the event references (`"0x140…"`). Opaque; for correlation within a session. |
| `jwt_exp` | int64  | no       | Unix-seconds `exp` of the JWT in use at event time. `0` = unknown. Used by post-rotation-auth-violation detector. |
| `reason` | string | no       | Free-form context. For `swap` events, holds `old=… new=…`. For `disconnect`/`closed`, holds the nats.go error string when available. |

### DiagReply (IPC + `--json`)

```jsonc
{
  "summary": {
    "state": "connected",                // string: connected / disconnected / connecting / unknown
    "state_since": "2026-06-02T00:34:02Z",
    "url": "nats://pipescloud.io:4222",
    "refresh_last_at": "2026-06-02T00:34:05Z",
    "refresh_next_due_at": "2026-06-02T00:39:05Z"
  },
  "patterns": [                          // detector hits; empty == clean
    {"name": "burst-swap-storm", "at": "...", "detail": "..."}
  ],
  "nats_state": "connected",             // duplicated for backward compat
  "nats_drops_last_hour": 2,
  "nats_events": [/* NATSEvent[] */],
  "on_disk_count": 247                   // active jsonl line count
}
```

### Storage layout

```
$PPZ_HOME/
  nats-events.jsonl          # active; one event per line; ≤ 10 MB
  nats-events.jsonl.1        # most recent rotation
  nats-events.jsonl.2        # oldest retained
  diagnostics-lifecycle.jsonl # daemon start/stop only; 32-line cap
```

Rotation: when the active file exceeds 10 MB, it's renamed to `.1`
(and prior `.1` to `.2`, prior `.2` discarded). Worst case on disk
is ~30 MB. Implemented in `nats_events_persistence.go`.

---

## 4. Event type vocabulary

Closed set. Extend only by adding code in `nats_events.go`'s doc
comment AND a row here AND (usually) a detector pattern that consumes
the new type.

| Type            | Emitted by                                | What it means                                          |
|-----------------|-------------------------------------------|--------------------------------------------------------|
| `connect`       | nats.go ConnectHandler                    | Initial connection established (rarely used today).    |
| `disconnect`    | nats.go DisconnectErrHandler              | Library noticed the TCP went away. `reason` = err.     |
| `reconnect`     | nats.go ReconnectHandler **or** ensureNATS | Connection (re)established. `caller="nats.go"` for the library callback; otherwise the daemon function that rebuilt it. |
| `closed`        | nats.go ClosedHandler                     | Connection fully closed; will not auto-retry.          |
| `swap`          | daemon `swapNC`                           | Daemon code installed a new NC and closed the old one. The most useful single event for "who replaced this connection?" — see §6. |
| `force_close`   | daemon `reportNATSFailure`                | A JetStream op failed AND a verification probe confirmed the connection dead (true zombie); the daemon is closing it. `reason` = the triggering error + the probe error. The `nats.go` disconnect/closed pair that follows is this close's fallout — never an orphan. |
| `warn`          | non-fatal failure paths                   | Today: `subscribeOrgHeartbeats` failure, and `reportNATSFailure` whose probe passed (connection kept; `reason` names the op error it absorbed). Add new sources sparingly. |
| `daemon_start`  | `Daemon.Run` enters                       | Process began.                                         |
| `daemon_stop`   | `Daemon.Run` defer                        | Process exited cleanly. Absence = crash.               |

---

## 5. Caller vocabulary

`caller` distinguishes daemon-initiated transitions from
library-initiated ones. Today's set:

| Caller                       | Meaning                                                                   |
|------------------------------|---------------------------------------------------------------------------|
| `nats.go`                    | nats.go's internal callback fired (disconnect/reconnect/closed).          |
| `handleLogin`                | `ppz daemon login` installed a fresh NC.                                  |
| `ensureNATS`                 | Send/read pre-flight built a new NC.                                       |
| `ensureNATS-refresh-due`     | `ensureNATS` saw a fresh creds rotation and dropped the prior NC.         |
| `OnRefreshed-callback`       | Refresh-loop callback proactively rebuilt NC after rotation.              |
| `watchState-creds-gone`      | File-watcher dropped NC because creds were deleted out-of-band.            |
| `subscribeOrgHeartbeats`     | (warn only) heartbeat subscription failed.                                 |
| `reportNATSFailure`          | (`force_close`/`warn` only) JetStream-op failure report: probe failed → closing, or probe passed → kept. |
| `?`                          | Pre-Phase-0 event reloaded from disk with no caller stamped.              |

When you read a trace: every NC transition should be attributable to
exactly one caller. Multiple non-`nats.go` callers swapping the NC
in the same second is the signature of a concurrency bug (see
§6 burst-swap-storm).

---

## 6. Patterns

Detectors live in `internal/daemon/nats_event_patterns.go`. Each is
a pure function from `[]NATSEvent` to `[]PatternHit`. The CLI runs
every detector against the visible window and prints hits as `⚠` lines.

### burst-swap-storm

**Fires when:** ≥3 `swap` events within 2s, of any callers.

**What it means:** Concurrent code paths are racing to replace the NC.
Today (Phase 0) the dominant cause is multiple IPC handlers calling
`ensureNATS` during a JWT rotation while the refresh-loop callback
(`OnRefreshed-callback`) is also rebuilding. Each swap closes the
previous (just-installed) NC, generating cascading `disconnect`+`closed`
pairs with empty reasons.

**How to confirm:** Look at the swaps' `caller` list in the detail.
Mixed callers (`OnRefreshed-callback`, `ensureNATS`,
`ensureNATS-refresh-due`) confirm the race. All-same-caller swaps in
a tight window are suspicious in a different way and warrant a closer
look (likely a retry loop).

**Fix scope:** Phase 1 of the agent-hardening plan (mutex on `d.NC`,
singleflight on `ensureNATS`, drop the double-act in `ensureNATS` when
`OnRefreshed` already installed an NC).

### post-rotation-auth-violation

**Fires when:** A `closed` event with `reason` containing
"Authorization Violation", preceded by a `disconnect` whose `jwt_exp`
is within 60s of the disconnect's wall-clock time.

**What it means:** The server kicked the connection at JWT expiry,
and nats.go's internal reconnect retried with creds the daemon had
not yet rotated — so the retry failed authentication. Either:

- The proactive `OnRefreshed` reconnect did not fire (or fired too
  slowly) before the server's kick, OR
- The refresh loop's `expUnix` disagrees with the server's view of
  `exp` (clock skew beyond `skewSeconds=30`).

**How to confirm:** Inspect the `jwt_exp` value on the prior
`disconnect` event. Compare to the `refresh.last_at` in the summary.
If `jwt_exp - refresh.last_at` is smaller than the typical JWT
lifetime, the refresh loop is firing later than expected. If the
disconnect's wall-clock time is AFTER `jwt_exp`, the server view is
ahead of the daemon's view by that delta.

**Fix scope:** Phase 1 (lengthen `skewSeconds`, harden refresh-loop
scheduling under load). Phase 2 (reconciler model) eliminates the
class.

---

## 7. Workflows

### A user reports "ppz says nats is unreachable"

Ask them to run: `ppz diagnostics --bundle` and attach the resulting
tarball.

When you open it:

```sh
tar tzf ppz-diag-*.tgz   # see what's inside
tar xzf ppz-diag-*.tgz -C /tmp/ppz-diag
cat /tmp/ppz-diag/MANIFEST           # what was captured
cat /tmp/ppz-diag/diagnostics.json   # summary + patterns at bundle time
jq -c '. | select(.type=="swap")' /tmp/ppz-diag/nats-events.jsonl | tail -50
```

The `MANIFEST` lists every file attempted, with a reason for any that
were unreadable.

### Reading a live session

`ppz diagnostics` for current state. If summary says `state: connected`
but `ppz send` returns `E_NATS_UNREACHABLE`, that's the
status-vs-operability disagreement documented in
`AGENT_HARDENING.md` (Phase 1 fix in progress).

If patterns surface anything, the `Detail` field tells you what to
look at. Don't paraphrase it from memory — it's machine-generated and
specific to the trace.

### Adding a new pattern detector

1. Write a `detector` func in `nats_event_patterns.go` returning
   `[]PatternHit`. Pure function; no side effects.
2. Register it in the `detectors` slice.
3. Add a unit test in the same file's `_test.go` with a synthetic
   event slice that exercises the detector both in matching and
   non-matching shapes.
4. Add a row to §6 of this document with the trigger, meaning,
   confirmation steps, and fix scope.

That's the contract — if your detector exists in code but not in
this doc, the next reader can't interpret its hits.

---

## 8. Versioning

`NATSEventSchemaVersion` is the source of truth. The persisted jsonl
stores `"v"` on every line so a reader can detect schema generations.
Today: 1. Bump on rename or semantic change of any field; new
omittable fields do not require a bump.

Old daemons writing v1 events should never appear in the same file
as new daemons writing vN events — the jsonl is per-daemon-home, and
a daemon only writes one schema version. Cross-version mixing only
matters if someone copies a bundle from one host to another, in
which case the reader should branch on `v`.
