---
name: ppz-pipes
description: "Communicate and coordinate between concurrent Claude Code agents over the ppz pipes mesh — pin a stable session identity, orient, send/read with read-receipts (acks), manage subscriptions, recover from drops, and run a lead/worktree/merge collaboration protocol for parallel multi-agent work in one shared repo."
---

# ppz — Pipes for Agents

## What This Skill Does

`ppz` is a durable message bus that lets several Claude Code agents (running
side-by-side on one or more machines) talk to each other and coordinate work.
Each agent owns a **handle** (e.g. `alice`, `agent2`); each handle exposes a
set of **pipes**. Agents publish messages to each other's pipes and read from
their own. Use this skill whenever you are launched as a named node on a ppz
mesh, or asked to coordinate with a peer agent.

It is the multi-machine / multi-agent cousin of the `a2a` skill (which is a
1:1 tmux-screen-scrape channel). Reach for **ppz** when there's a real daemon
and named handles; reach for **a2a** when two sessions just share a terminal
multiplexer.

> Transport is NATS/JetStream behind a local daemon. The daemon can drop and
> reconnect under you — see **Resilience** below. This skill is verified against
> ppz **v0.45**; run `ppz version` and `ppz <cmd> --help` if something here
> doesn't match your binary.
>
> Need to run your **own** server instead of pipescloud.io (private/air-gapped
> mesh, CI, e2e)? See [`running-a-local-server.md`](./running-a-local-server.md)
> — self-hosting (Postgres + embedded NATS + auth), via the interactive
> `scripts/ppz-local-server.sh` helper or a manual Compose / native boot.

---

## STEP 0 — Pin your session identity (do this FIRST, always)

**This is the one step that makes everything else work for an agent.** Claude
Code runs every `ppz` call as a *separate subprocess with no shared TTY*. ppz
keys the "current handle" off the calling tty, so without a pinned session each
subprocess gets a *fresh* session id — your current handle is lost between
calls, `send` loses sender attribution, and `send --request-ack` rejects with
`E_NO_CURRENT_SOURCE` (exit 16).

Export a stable session id once, at the agent's lifecycle level, before any
other ppz call:

```bash
export PPZ_SESSION="agent2"     # any stable id; reuse it for the whole session
```

Then **verify** your identity actually stuck — don't assume:

```bash
ppz status        # the 'current source:' line must show YOUR handle, not '-'
```

If `current source:` is `-`, set it explicitly and re-check:

```bash
ppz set handle agent2 && ppz get handle
```

(See `ppz help sessions` for the canonical explanation.)

---

## Mental Model

```
handle "agent2"                       handle "alice"
├── agent2.inbox      <-- messages    ├── alice.inbox
├── agent2.heartbeat      to you      ├── alice.heartbeat
├── agent2.stdctrl                    ├── alice.stdctrl
├── agent2.stdin                      ├── alice.stdin
└── agent2.stdout                     └── alice.stdout
```

- **`inbox`** is where peer-to-peer messages land. This is the only pipe you
  normally *action*. A bare handle (`ppz send alice …`) targets `alice.inbox`.
- **`heartbeat` / `stdctrl` / `stdin` / `stdout`** are transport / telemetry /
  pty pipes managed by the daemon — **not** work items. Don't treat unread
  counts on these as tasks.
- A message has an `id`, a `to=<pipe>` target, an optional `subject`, and a
  `bytes` size. Hard limit: **65,536 bytes per envelope** (see Payload limits).
- You can also create your own pipes (`ppz pipe create [H.]NAME`) and group
  pipes under a namespace/manifold (`ppz set namespace PATH`) — rarely needed
  for simple peer coordination.

---

## Orientation (run these after Step 0, when launched on a mesh)

```bash
ppz who          # agents the daemon has seen heartbeats from
ppz subs ls      # your subscriptions: NAMESPACE PIPE UNREAD BUFFERED LAST PAYLOAD CREATOR
ppz status       # daemon state, current handle, last token refresh, nats state
```

Then confirm the channel works by sending your launcher a short hello and
asking for a read-receipt:

```bash
ppz send alice "Hello from agent2 — channel confirmed. Standing by." --request-ack
# success line is printed to STDERR (since v0.25); exit 0 == delivery confirmed.
```

---

## Core Messaging Commands

| Command | Effect |
|---|---|
| `ppz send TGT "PAYLOAD" [--subject S] [--in-reply-to ID] [--request-ack] [--priority P]` | Publish to pipe `TGT` (bare handle ⇒ `<handle>.inbox`). |
| `ppz read TGT` | Read **new** messages on `TGT` and **advance the cursor**. `ppz read inbox` reads `<current>.inbox`. |
| `ppz read TGT --tail` | Drain unread then keep streaming live until SIGINT (advances cursor). |
| `ppz reread TGT` | Replay retained messages — **never** moves the cursor. Carries `-l/--skip/--since`. |
| `ppz ls [--watch] [PAT]` | List handles × pipes; `--watch` **blocks until unread** arrives (firehose across every pipe). |
| `ppz subs read` | Read new messages across **subscribed** pipes only. |
| `ppz subs ls` | List your subscriptions and their unread/buffered counts. |
| `ppz subs add/rm/wait TARGET` | Manage subscriptions; `wait` blocks until a *subscribed* pipe has unread. |

### `read` vs `reread` (the #1 gotcha)
`ppz read` is **destructive to the cursor**: once read, a message won't reappear
in the next `read`/`subs read`. If you need to re-examine history, use
`ppz reread TGT`. If you `read agent2.inbox` directly, a later `subs read`
won't show that message again.

### `subs read`/`subs wait` only cover *subscribed* pipes
By default you're subscribed to your own `inbox`. `ppz subs read` returns
nothing if there's nothing new **on a subscribed pipe** — it does not scan the
whole mesh. To see everything, use `ppz ls`. `ppz ls --watch` is the firehose;
`ppz subs wait` is the curated equivalent.

### Read receipts — `--request-ack` (prefer over guessing)
When you need to know a peer actually *saw* your message, send with
`--request-ack`. Their daemon auto-emits an `ack:read` envelope back to **your**
inbox carrying `in_reply_to=<your-msg-id>`; the tabular reader renders it as
`ack:read → <id8>`, so you can correlate at a glance. This is **best-effort and
non-blocking** — a missing ack is indistinguishable from "not yet read", so for
strict guarantees layer your own re-send-on-timeout. The `ack:` subject prefix
is **reserved**; setting it yourself errors with `E_INVALID_SUBJECT` (exit 23).
Use `--in-reply-to ID` to thread human replies to a prior message. (`ppz help acks`.)

### Message priority — `--priority 1|high, 2|medium, 3|low`
Stamp urgent sends with `--priority high`; the recipient's `read`/`subs read`
drain delivers higher tiers first (FIFO within a tier). Omitted = medium.
Two rules to work by:
- **Priority only reorders a backlog.** It applies within a single drain — if
  you read one message at a time (the `subs wait` → `read` loop), there is no
  backlog and nothing to reorder. `--tail` and byte-faithful pipes
  (stdout/stdin/stdctrl/custom) always stay in arrival order.
- **Trust the field, not the text.** High/low rows carry a `P1 `/`P3 ` body
  prefix in tabular output, but that text column also contains sender-
  controlled payload — any peer can send a payload that *starts with* "P1 ".
  When priority matters to your decision, read with `--json` and use the
  structured `priority` field (or simply trust delivery order); never act on
  the inline badge text.

### Standard poll loop
When told to "stand by", a clean idempotent check is:

```bash
ppz subs read 2>&1; echo "===INBOX==="; ppz read inbox 2>&1; echo "===SUBS LS==="; ppz subs ls 2>&1
```

For hands-off waiting, **block instead of busy-polling**:

```bash
ppz ls --watch agent2.inbox     # blocks until something lands (firehose)
# or, scoped to your subscriptions:
ppz subs wait                   # blocks until a subscribed pipe has unread
```

> Note: a blocking `ppz read --wait --timeout` is **not** implemented yet. The
> supported blockers are `ls --watch`, `subs wait`, and `read --tail`. If a
> blocking call returns on a connection drop rather than a message, treat it as
> a transient and re-issue (see **Resilience**).

---

## Payload limits

The envelope hard cap is **65,536 bytes**; exceed it and `send` fails with
`E_PAYLOAD_TOO_LARGE` (exit 17). Beyond the hard cap, the `ls`/`subs ls`
`PAYLOAD` column **truncates** for display.

**Don't pipe artifacts through `send`.** Send a *pointer*, not the payload:
a commit sha, a branch name, a file path, a test count. Put the actionable verb
up front so it survives truncation:

```bash
ppz send alice "IMPLEMENTED @ a1b2c3d — graph_analyzer tie-break; suite 142 passed."
# NOT: ppz send alice "<2KB of diff/log output>"
```

---

## Resilience (long-running agents WILL hit drops)

The daemon sits on NATS/JetStream and can lose + recover its connection. Build
for it:

- **Re-verify identity after any daemon hiccup.** A daemon restart **wipes the
  per-session `current` map from memory** — your `current source` silently
  becomes `-`. Before any coordinate-dependent op (especially `--request-ack`),
  glance at `ppz status` and re-`set handle` if needed.
- **`E_NATS_UNREACHABLE` (exit 19) is ambiguous.** It can mean genuine network
  loss *or* expired credentials. On a persistent failure, check
  `ppz status` (token refresh / nats line) and re-`ppz login` if creds lapsed,
  rather than retrying blindly forever.
- **Retry transient failures with backoff.** Wrap sends/reads so a single
  `E_NATS_UNREACHABLE` / `E_DAEMON_NOT_RUNNING` retries a few times with growing
  delay before you escalate to the user.
- **`ppz diagnostics`** introspects the daemon and works *without* login —
  reach for it when `status` itself is unhappy.

---

## Identity & Daemon (occasionally needed)

```bash
ppz source create H        # claim a bare message handle (auto-creates H.inbox) — NO harness
ppz terminal share H [-- CMD]  # run a pty (shell/CMD) bound to H — publishes H.stdout, reads H.stdin
ppz agent create H         # create a handle AND run an AI harness in it
ppz set handle H           # set this session's current handle
ppz get handle / unset handle
ppz pipe create [H.]NAME   # create a custom pipe;  pipe destroy [--recursive]
ppz daemon start|stop|restart|logout
ppz source destroy 'PAT'   # glob-destroy sources/pipes (careful — destructive)
```

**Which verb?** `source create` = a bare mailbox handle, no process, set
current. `terminal share` = run a live pty (shell/CMD) bound to a handle —
auto-creates the handle if missing, publishes its stdout and reads its stdin.
`agent create` = a handle *plus* a spawned AI harness running in it. For "I
just need to talk on the mesh", you usually already have a handle (pinned via
`PPZ_SESSION`) and need none of these.

### `ppz command H` — driving another agent's keyboard (DANGEROUS)
`ppz command H "INSTR"` types `INSTR` into `H.stdin` then sends a control key —
i.e. it injects keystrokes into a peer's live prompt. This is **outward-facing
and hard to reverse**:

- **Get explicit consent first.** Never use it to interrupt an agent that is
  mid-task — you may corrupt a partial prompt or land input in the wrong field.
- Prefer a normal `ppz send H "<request>"` to the peer's inbox and let *it*
  decide to act. Reserve `command` for cases the peer has agreed to.

---

## Multi-Agent Collaboration Protocol (lead / worktree / merge)

This is the battle-tested pattern for "two agents each improve one shared repo
in parallel". The **ppz mechanics** below are generic; the **git policy** block
that follows is specific to Andy's CLAUDE.md and should be swapped out on other
setups.

**Roles.** One agent is **lead** (owns the merge baton); others are workers.
The lead announces itself and asks for an ACK (send with `--request-ack` so you
also get a protocol-level read-receipt, not just the prose reply).

**1. Negotiate scope before implementing.** Each worker proposes ONE change as
`FILE(s) — what — why` and waits for the lead's overlap check. Pick disjoint
files; flag any shared *runtime* interaction (a function the other will call),
not just shared files. Reply format the lead expects:

```
ACK lead. PROPOSAL: <file> — <change> — <why>
```

**2. Implement on your own branch, then report** a one-line status with the
actionable verb up front (keep it under the payload cap — pointer, not diff):

```bash
ppz send alice "IMPLEMENTED @ <sha> — <one-line summary>; full suite N passed." --request-ack
```

**3. Lead merges**, runs the full suite + lint gate on the merged branch, then
broadcasts `DONE on main @ <sha>` (or the integration branch).

**4. Workers independently verify** the merge rather than trusting the report —
read-only against refs, then `ppz send <lead> "VERIFIED ✓ …"` with what you
checked.

**Status verbs the lead/Andy expect:** `ACK …`, `DONE on main @ <sha>`,
`IMPLEMENTED @ <sha>`, `VERIFIED ✓ …`, `BLOCKED: <reason>`. Use them.

---

### Local git policy (Andy's CLAUDE.md — replace on other machines)

Maps onto: per-task feature branch, ff-merge into main allowed, state-changing
git delegated to a Haiku subagent (`git-delegation`).

- **Isolate with worktrees** — agents share one checkout, so each works in its
  own git worktree to avoid a `.git` lock race and file collisions. Stagger
  creation (lead first, signals GO, then workers):

  ```bash
  git worktree add ./.worktrees/agent2 -b agent2/improvement
  ```

- **Commit via the Haiku git subagent**, not inline.
- **Lead integration**: ff-merge the linear branch, 3-way merge the other;
  disjoint files ⇒ zero conflicts.
- **Worker verification** uses a throwaway detached worktree so the shared tree
  is never disturbed:

  ```bash
  git rev-parse --short ppz-improvements
  git log --oneline --graph main..ppz-improvements
  git diff --stat main..ppz-improvements
  git worktree add --detach ./.worktrees/_verify <merge-sha>
  ( cd ./.worktrees/_verify && uv run pytest -q && uv run ruff check <changed-files> )
  git worktree remove ./.worktrees/_verify --force   # then: rm -rf the dir; git worktree prune
  ```

---

## Error codes (exit code → meaning → what to do)

`ppz` exits non-zero with a stable code. The ones an agent actually hits:

| Exit | Code | Meaning & fix |
|---:|---|---|
| 10 | `E_NOT_LOGGED_IN` | No stored credentials → `ppz login <url>`. |
| 11 | `E_DAEMON_NOT_RUNNING` | IPC socket down → `ppz daemon start` (then retry). |
| 13 | `E_SOURCE_TAKEN` | Handle already claimed → pick another, or it's already yours. |
| 14 | `E_SOURCE_NOT_FOUND` | Target handle/source doesn't exist → check `ppz who`. |
| 15 | `E_INVALID_HANDLE` | Handle fails regex / reserved → rename. |
| 16 | `E_NO_CURRENT_SOURCE` | No current handle → **export `PPZ_SESSION`** + `ppz set handle H` (Step 0). |
| 17 | `E_PAYLOAD_TOO_LARGE` | Envelope > 65,536 bytes → send a pointer, not the artifact. |
| 18 | `E_SERVER_UNREACHABLE` | Can't reach ppz-server → check network / `ppz status`. |
| 19 | `E_NATS_UNREACHABLE` | Can't publish → may be net **or expired creds**; check `status`, re-login, retry w/ backoff. |
| 20 | `E_INVALID_PIPE` | Malformed pipe name → fix `<handle>.<pipe>` form. |
| 23 | `E_INVALID_SUBJECT` | Reserved-prefix violation (`ack:` is system-only) → drop the prefix. |

---

## Gotchas & Conventions

- **Pin `PPZ_SESSION` first.** Without it, attribution and acks silently break.
- **System pipes ≠ tasks.** Unread on `heartbeat`/`stdctrl`/`stdout` is noise;
  only `inbox` carries work. Don't action telemetry.
- **`read` advances the cursor** — use `reread` to re-examine.
- **`subs read`/`subs wait` are scoped** to subscriptions, not the whole mesh.
- **Acknowledge, don't act, on confirmations.** Many inbox messages are "noted
  / stand by" — reply or stay silent; don't manufacture work.
- **`send` success goes to STDERR** (since v0.25); exit 0 means delivery was
  confirmed. Scripts redirecting stdout no longer swallow the line.
- **Worktree cleanup leaves dirs behind.** `git worktree remove` can fail with
  "Directory not empty" if pytest wrote `__pycache__`/`.pytest_cache`; follow
  with `rm -rf <dir> && git worktree prune`.
- **Keep payloads single-line-ish** with the actionable verb up front
  (`IMPLEMENTED @ …`, `BLOCKED: …`, `DONE on main @ …`).

---

## Quick Reference

```bash
export PPZ_SESSION="agent2"          # STEP 0 — pin identity (once, first)
ppz status                           # confirm 'current source:' is YOU, not '-'
ppz who                              # who's online
ppz subs ls                          # my subscriptions + unread
ppz send alice "..." --request-ack   # message a peer, ask for a read-receipt
ppz read inbox                       # read my new mail (advances cursor)
ppz reread inbox                     # replay without moving cursor
ppz ls --watch agent2.inbox          # block until mail arrives (firehose)
ppz subs wait                        # block until a subscribed pipe has unread
ppz diagnostics                      # introspect daemon (works without login)
```
