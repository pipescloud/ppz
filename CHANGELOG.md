# Changelog

## Unreleased — pipe ACLs, phase 3 (opt-in enforcement)

ACLs now actually restrict access — **but only for an org that has switched
enforcement on**, and enforcement is off for every org, existing and new.

- **Per-org opt-in** (`accounts.acl_enforced`, default false). `/auth/exchange`
  returns the same wide-open credential as today without touching the ACL
  tables at all until an org opts in. ACL defaults are derived rather than
  stored, so flipping enforcement globally would have made every shared
  terminal private on upgrade morning with no warning.
- **New Security tab** on the org page. Off: a preview of what enabling would
  break, and an Enable button. On: the live rights table and Disable.
  Disabling keeps grants, so switching back restores exactly the previous
  configuration.
- **The preview** is computed from the derived defaults, not from observed
  traffic — traffic is wrong in both directions, silent on a collaboration
  that happens to be idle and noisy about a one-off read months ago. It leads
  with handles whose owner has left the org: those become reachable only by an
  org owner or admin, and the nominal owner loses them too, which is the
  failure that otherwise looks like nothing in particular.
- **Credential compilation.** Reads compile to JetStream API entries, writes to
  a bare subject publish — disjoint sets, which is what makes write-without-read
  enforceable rather than merely declarable. Stream enumeration is denied
  outright (`STREAM.LIST`/`NAMES` carry no stream token, so they cannot be
  scoped per pipe), and stream lifecycle is denied even for admin, closing the
  JS-API control-plane hole noted in `docs/AUTH-V2.md` §3.5.
- **Every ACL surface reports whether it is enforced.** An answer that reads as
  a guarantee while nothing upholds it would be worse than no answer.
- **New verbs:** `ppz acl enforce [on|off]`, `ppz acl preview`, both with
  `--json`.

## Unreleased — pipe ACLs, phases 1-2 (principals, grants, visibility)

Builds on phase 0. Grants are stored and every surface can show them, but
**nothing is enforced yet** — the credential compiler is phase 3. Until then
an ACL is advisory: it describes intent and the GUI/CLI honour it, while a
hand-rolled NATS client still is not stopped.

### Principals

- **Org roles gain an `admin` tier** (`account_members.role`). Admin can do
  what an owner can except transfer ownership and change roles — deliberately
  below owner, so widening the org gates cannot let an admin promote
  themselves. Unknown role values fail closed.
- **Service accounts** (`users.mode='service'`) give an agent its own
  identity, distinct from the human who spawned it: a real principal that
  holds grants, owns handles and is attributed on `ppz who` and on every
  message it publishes. `POST /api/v1/svc`, `GET /api/v1/svc`,
  `DELETE /api/v1/svc/{name}`, `POST /api/v1/svc/{name}/keys`.
- **A key can act as someone other than its minter.** A service key is
  created_by a human and acts_as the bot; rows it creates now attribute to
  the principal (`db.APIKey.Actor()`), not the human. Crediting the minter
  would misattribute the agent's work and, once enforcement lands, evaluate
  the wrong subject.

### ACLs

- `read` and `write` are **independent** — neither implies the other. `admin`
  implies both plus managing the pipe. Write-without-read is the shape
  `<handle>.inbox` needs, and it is enforceable rather than merely
  declarable because NATS keeps publish and the JetStream API in disjoint
  permission sets.
- **Defaults are derived, never stored.** The collar is the ownership
  boundary: everything under a handle is that handle's principal's.
  `<handle>.inbox` takes writes from anyone; `<handle>.heartbeat` is its dual
  (readable by everyone, written only by its owner); stdio and user-created
  collared pipes are owner-only; uncollared pipes are shared org space.
  Because there is no stored blanket grant to subtract from, there are no
  deny rules and no precedence tiers — every stored row is an allow.
- **Every view reports provenance**, not the raw grant table. Most access has
  no row behind it, so a view built on `acl_grants` renders almost nothing
  and implies that nobody can reach `<handle>.inbox`.
- **New verbs:**
  `ppz pipe acl ls PIPE`, `ppz pipe acl grant PIPE PRINCIPAL PERM`,
  `ppz pipe acl revoke PIPE PRINCIPAL [PERM|all]`, `ppz acl whoami PIPE`,
  `ppz acl ls --principal NAME`. All take `--json`.
- **A denial says how to fix itself.** `ppz acl whoami` on a pipe you cannot
  reach prints the exact grant command and the principals able to run it, so
  an agent can ask over that principal's inbox instead of failing opaquely.
- **Roster visibility:** anyone holding any access on a pipe can see who else
  can — including write-only. An inbox sender holds no read, and a
  "can you read it" gate would hide the roster from every sender in the org.

## Unreleased — pipe ACLs, phase 0 (identity + presence isolation)

Groundwork for per-pipe ACLs (`docs/ACL.md`). No ACL is enforced yet; these
are the two prerequisites that made enforcement impossible, both of which
were defects in their own right.

- **API keys now act as a principal.** A key carried `created_by_user_id`
  for row attribution only — the authenticated caller itself had no
  identity (`AuthedCaller.UserID` was `uuid.Nil` on the API-key path), so
  every handler needing a user rejected API keys outright with "this
  endpoint requires an OAuth token". New `api_keys.principal_user_id`
  (migration 0007, backfilled from the creator) is the identity a key acts
  as, and both auth surfaces now populate the caller uniformly. Invite
  endpoints consequently work with an API key, gated on the key's principal
  rather than refused.
- **Heartbeats no longer ride the org firehose.** The daemon collected
  presence by core-subscribing to `<account>.>` and filtering for a
  `.heartbeat` suffix client-side. Live JetStream publishes are delivered
  to core subscribers too, so that one subscription received **every**
  message published anywhere in the org — every byte of every shared
  terminal, every inbox message between other agents. Presence moves to its
  own subject family, `<account>._presence.<manifold?>.<handle>`, and the
  daemon subscribes to exactly that.
- **Wire change:** heartbeat subjects and their backing streams are renamed
  (`presence_<org>_<handle>`). Routed inside `natsubj.BuildSubject` /
  `BuildStreamName`, so provisioning, publishing, reading, `ppz ls` and the
  GUI chat roster all follow. `heartbeat` becomes a reserved pipe name.
- `natsubj.AutoProvisionedPipes` is now the single source of truth for the
  auto-provisioned set — it had drifted, still listing the pre-launch
  `broadcast` and omitting `heartbeat` and `system`. `ppz pipe destroy`
  glob expansion uses that set to skip auto-pipes, so the stale set would
  have let a glob match `<handle>.heartbeat`.
## Unreleased — `ppz ls -l` shows pipe retention

**Retention became readable.** `ppz pipe set` could change a pipe's caps but
nothing could report them back: they were echoed once in the reply line at the
moment of the change and then invisible. `ppz ls -l` (`--long`, after `ls -l`)
adds `TTL` / `MAXMSGS` / `MAXBYTES` between `BUFFERED` and `LAST`, so each cap
sits beside the count it bounds.

- **Values come from the JetStream stream config, not the `pipes` table** —
  the thing actually enforcing the caps. That is also the only way to answer
  for auto-provisioned pipes: `inbox` / `stdout` have no row until someone
  runs `pipe set` on them, yet they are the pipes whose defaults bite first.
- **No new endpoint and no extra round trip.** The daemon already holds a
  `*jetstream.StreamInfo` per pipe — `BUFFERED` and `LAST` come from its
  `State`, and the caps sit in its `Config`.
- **`--json` is unchanged unless `-l` is given.** Agents parse that output, so
  adding keys to its default form would be a wire change for every existing
  consumer. `ttl_seconds` / `max_msgs` / `max_bytes` appear only under `-l`,
  and the daemon does not populate them otherwise — the gate is on population,
  not rendering.
- **Caps render as raw integers**, because `pipe set --max-bytes` parses only
  integer mantissas: a humanised `1.4MiB` would print a value that fails when
  pasted back into the command that set it. `TTL` does collapse to `24h` /
  `5m`, and JetStream's `-1` "unlimited" renders `∞`.
- **Under `-l` the JSON schema is fixed**, including when a value is 0, so a
  consumer can tell "long mode, no age limit" from "not long mode".
- `ppz subs ls` keeps the short table — it answers "what am I subscribed to",
  not "how is this pipe configured".
- Note the deliberate flag collision: `-l` is `--long` on `ls` but `--limit`
  on `read` / `reread`. The `ls -l` spelling is near-universal muscle memory
  and worth the inconsistency.

## Unreleased — configurable pipe retention (`ppz pipe set`) + audit trail

**Retention is no longer fixed at create time.** `ppz pipe set [HANDLE.]NAME`
changes an existing pipe's retention, with the same target grammar and the
same flag names as `ppz pipe create` — one vocabulary, not two:

```
ppz pipe set chat.archive --max-msgs=500
ppz pipe set chat.archive --ttl=168h --max-bytes=64MiB
```

- **Fields you don't name keep their value.** The server merges the request
  onto the stored row, so `--ttl` doesn't quietly reset a previously
  configured `--max-msgs` back to the default. Naming no flag at all is an
  error rather than a silent no-op.
- **Auto-provisioned pipes are now configurable.** `inbox`, and
  `stdin`/`stdout`/`stdctrl`/`system`/`heartbeat` on terminals, have no
  `pipes` row — which is why their caps were previously unreachable, despite
  being the caps users hit first. `pipe set` materialises a row on first
  override (stamped with the *source's* creator, so `ppz ls` CREATOR doesn't
  get reassigned by a retention change).
- **Lowering a cap discards immediately** — shrinking `--max-msgs` below the
  retained count drops the oldest messages there and then.
- The printed line states the pipe's complete retention afterwards, not just
  what moved: `updated pipe=chat.archive retention=ttl=24h0m0s,msgs=500,bytes=16777216`.

**Bug fix: stream config changes were silently dropped.** Stream provisioning
called `CreateStream` and swallowed `ErrStreamNameAlreadyInUse`, so
re-provisioning an existing stream with a different config did nothing. Any
retention change to a live pipe was a no-op, and bumping the built-in defaults
never reached streams already in existence. Now `CreateOrUpdateStream`.

**Bug fix: re-provisioning silently reverted a `pipe set`.** The switch to
`CreateOrUpdateStream` cuts both ways: any path that re-provisions at the
*built-in defaults* now overwrites a tuned stream instead of no-opping on it.
Bare `ppz terminal share` re-provisions the whole pty pipe set on every
invocation, so a configured `chat.inbox` went back to 5000 messages while
postgres went on reporting 3. Source creation, the pty promotion and account
(re)open now share one override-aware provisioning helper, and the
defaults-only one is gone rather than left around as a footgun.

**Bug fix: `ppz pipe destroy '*'` could destroy a terminal's control plane.**
The glob-expansion skip list was missing `system` and `heartbeat`, two names
`Source.Pipes()` genuinely auto-provisions. Previously unreachable (nothing
could put those names in the user-pipe list); `pipe set` made it reachable.

**Retention resolution now lives in one place.** `resolveRetention` takes
layers highest-precedence-first and resolves each field independently, so a
pipe overriding only `max-msgs` still inherits the default TTL. This replaces
three open-coded nil-check ladders, and makes org/account-level defaults a new
layer rather than a rewrite at every call site.

**New: audit trail, on an owner-only org tab.** `/orgs/<slug>/audit` shows a
newest-first log of pipe lifecycle mutations — `pipe.create`, `pipe.set`,
`pipe.destroy` — with the change rendered as a delta (`msgs 5000 → 5`) rather
than a bare "something changed".

- Backed by a generic `audit_events` table (migration `0006`): actor, action,
  target, before/after jsonb. Pipe actions are its first writers; key revoke,
  member removal and source destroy fit the same row shape.
- **Actor attribution is honest about its limits.** On the API-key path the
  server knows only the key's *creator*, not who typed the command, so a
  shared org key attributes every change to whoever minted it. Rows record the
  key id and render `via api-key` vs `via web` so they aren't read as stronger
  evidence than they are.
- Covers both the collared (`/sources/{handle}/pipes/...`) and the uncollared
  (`/pipes`) endpoints, so an uncollared `pipe destroy` leaves a trail too.
- **Known gaps:** audit writes are best-effort (a failed insert is logged, not
  surfaced, because the mutation has already committed); the table has no
  retention policy yet; and `via web` is not yet reachable, since retention is
  currently only mutable from the CLI.

## Unreleased — once-only `.stdin` delivery (no command replay on resume)

**Bug fix (`ppz terminal share`).** A `ppz command` that a pty session
had already received and executed was delivered AGAIN when a new share
process started on the same handle. Field-observed on v0.56.10: a
`/compact` issued at 09:59:35 and executed at 10:01:25 was relayed a
second time at 08:26 the next morning — a 22.5 hour gap. It failed to
run only because that harness happened to be closed.

This bypassed every consent guard, because the guards run before the
send and not on delivery: approval given for one moment produced an
action most of a day later, on an agent nobody had asked.

- **Root cause.** The host followed `.stdin` with `NoAdvance=true`, so
  the daemon's cursor never moved and every Read re-drained the pipe
  from its first retained sequence. The only thing suppressing that
  replay was `seenIDRing` — an in-memory, in-process ring. A new share
  on an existing handle began with an empty ring and re-fed the child
  every `.stdin` message still inside the 24h retention, oldest first,
  submit sequences and all.
- **Fix.** The follow is now cursor-advancing and keyed to a session
  derived from the handle, so the watermark belongs to the agent rather
  than the shell that started it. A resumed share picks up after
  whatever an earlier one already fed the child. The watermark persists
  under `<PPZ_HOME>/cursors/`, so it survives daemon restarts as well as
  share restarts.
- **The live consumer honours the cursor too.** `handleRead` derived the
  follow's start sequence solely from what its retained-drain delivered.
  When the cursor already covered the whole window — the caught-up case,
  which is every ordinary resume — the drain was skipped and the live
  consumer restarted from the head of the stream, re-delivering
  everything the cursor had just excluded. The start sequence is now
  floored at the cursor. This defect hid whenever anything newer than
  the cursor was outstanding, because that takes the drain path.
- **A first share no longer inherits a backlog.** A watermark only
  protects a handle once one has been written, so the first share after
  upgrading (or on a wiped `PPZ_HOME`, a new machine, or a brand-new
  handle) would still have drained the full 24h window. With no cursor
  the daemon now seeks by time, to the moment the host started
  following: older is backlog the agent has already run, newer is owed
  to it. A command issued to a handle before any host existed for it is
  dropped rather than executed late.

  The floor is a time rather than "skip to the latest sequence" because
  the pipe is provisioned before the host dials, so a send can land
  before the follow is established — `ppz terminal share agent & ppz
  command agent …`, and every ordinary share startup. Skipping to the
  end silently discarded those. It is stamped once per host, not per
  dial: `ppz daemon logout` removes `<PPZ_HOME>/cursors` mid-process, so
  a dial can find no watermark with the pipe live and messages genuinely
  outstanding; redelivery back to the host's start is safe because the
  in-process dedupe ring suppresses whatever already reached the PTY.
- **The host's watermark is isolated from the agent's own reads.** The
  cursor namespace is deliberately not `<handle>`, which is what the
  host exports as `PPZ_SESSION` into the wrapped child: sharing it would
  let an agent that reads its own `.stdin` — directly or through a
  matching subscription — advance the host's watermark and silently
  consume commands the host then never delivered. The follow also stamps
  `Sender`, so the `ack:read` it now emits is attributed to the agent
  rather than to whatever `State.Current` resolves to (empty, for a pty
  source).
- **At-most-once, deliberately.** The daemon advances as it writes to
  the socket, so a host killed mid-drain drops what was in flight. For
  a channel whose messages execute on arrival that is the right trade:
  a lost keystroke can be retyped, a replayed one is an action nobody
  authorised.
- `seenIDRing` stays as the intra-connection guard, covering the window
  between the daemon advancing the cursor and the bytes reaching the
  PTY — it is no longer what stands between the retained window and the
  child.
- Pinned by `share-stdin-command-not-replayed-on-resume`, which counts
  executions rather than echoes and also asserts the resumed session
  still accepts live input. Its barrier waits for the resumed child
  itself: an earlier version published its marker before the resumed
  host had connected, which left something newer than the cursor
  outstanding and so passed against a daemon that replays. Backed by
  `TestHandleRead_Follow_DoesNotReplayPastCursor`,
  `TestHandleRead_Follow_SeedLatest_SkipsBacklogOnFirstRead` and
  `TestForwardStdinRequestIsolatesHostCursor`.

## Unreleased — fresh idle window for consumed episodes

**Bug fix (`ppz terminal share`).** A message arriving after the agent
had consumed the pending episode inherited the REMAINDER of the idle
window the previous message opened, instead of getting its own.
Field-observed on v0.56.6: msg-1 armed the 60s gate, the agent's
monitor read it, msg-2 landed 57s in — and was nagged five seconds
after arriving. Same root as the ladder bug (consumption is invisible
between fire attempts, so `pendingSince` went stale exactly the way
`unacked` did), expressed through the idle gate.

- **Consumed-episode deferral.** At a fire attempt whose confirm shows
  the watermark advanced during the pending window AND whose surviving
  unread is young (newest unread `LastAt` within `IdleAfter`), the
  injection is suppressed and the idle window restamped: the young
  message earns a full 60s of its own — usually resolving silently via
  the agent's own read, exactly like any fresh message.
- **One read buys one deferral.** The suppression adopts the confirm's
  watermarks as the new baseline, so further arrivals without further
  reads fire at the fresh gate regardless of youth — traffic alone can
  never starve the nag, and a non-reading agent still fires at the
  original 60s.
- **Old survivors fire at the gate** (the post-cooldown shape):
  deferring a message that already waited minutes would add latency
  for nothing. Unprovable youth (no `LastAt` in the snapshot) also
  fires — at-least-once preserved.
- The deferral's consumption proof resets the backoff ladder, and the
  arm-time baseline comes from the subs-wait wakeup's own row state —
  no new IPC anywhere.

## Unreleased — subs alert ladder resets on consumption

**Bug fix (`ppz terminal share`).** The repeat-alert backoff ladder
(previous entry) climbed monotonically for a fully RESPONSIVE agent.
Field-observed on v0.56.5: alert gaps grew 5m → 10m → 20m while the
wrapped agent read and replied to every message between alerts, leaving
each new message waiting up to 20 minutes for its nudge.

The old reset needed a fire-time confirm to catch the unread level at
exactly zero — but fire attempts happen only minutes apart, and under
conversation-style traffic a fresh message is always unread by then, so
consumption was never observed.

- **Consumption is now derived from a per-pipe watermark** —
  `Total - Unread` in the snapshot the fire-time confirm already
  fetches (no new IPC, no wire change). Any watermark advance since the
  last injected alert proves the agent read something; the ladder
  resets and that alert counts as a fresh episode's first.
- **Arrivals still never reset**: a new message moves Total and Unread
  in lockstep, leaving the watermark unchanged. Partial reads (the
  head-10 flood-cap page) advance it and count. Watermark decreases
  (retention expiry of read messages) and freshly subscribed pipes'
  pre-existing history do not.
- **Baseline moves only at injection, from a successful confirm**: a
  deferred fire (user typing) cannot destroy the advance its confirm
  observed, and an IPC error retains the last good baseline so a read
  around a daemon hiccup is still credited.
- The in-flight backoff window itself is never shortened — the fix
  stops wrongful climbing, so a responsive agent simply never inherits
  a long window.

## Unreleased — subs alert backoff (bounded nagging)

**Behaviour change (`ppz terminal share`).** The unread-subs alert the PTY
wrapper injects into a wrapped agent now fires on a **backoff ladder** instead
of a flat 30s cooldown, and the first nudge waits **60s** instead of 15s.

Flood protection for agent consumers, the injection-side counterpart to the
read flood cap below. An agent that *cannot* action its messages — session
usage limit, wedged REPL, crashed harness — used to be fed one
injected-and-submitted turn every 30s for as long as the condition lasted.
Nothing in the pump could tell a first nudge from a six-hundredth: the
fire-time unread check truthfully reports "still unread" every time, because
it is. A 5-hour usage-limit block queued ~600 copies of the same nag, and the
whole backlog flushed into the model as real turns the moment the limit
reset — burning context window and token budget.

- **Repeats escalate.** The gap after the Nth consecutive unacknowledged alert
  is `min(5m * 2^(N-1), 30m)`: 5m, 10m, 20m, then every 30m. A 5-hour block
  now costs 12 injections rather than ~600.
- **Only consumption resets the ladder.** The rung counts alerts *injected*
  since the unread level was last observed to drop — the one signal that
  proves the agent acted. Message **arrivals** deliberately do not reset it,
  or a busy pipe would reproduce the flood in full. Alerts that are suppressed
  (already read) or deferred (user typing) inject nothing and consume no rung.
- **First nudge at 60s.** Agents mostly watch their own subs, so the wider
  window lets the agent's own `ppz subs read` land first — the fire-time check
  then suppresses the nudge entirely and nothing reaches the PTY.
- Cadence is tunable via `PPZ_TERMINAL_INBOX_IDLE_MS`,
  `PPZ_TERMINAL_INBOX_COOLDOWN_MS` and the new
  `PPZ_TERMINAL_INBOX_COOLDOWN_MAX_MS` (ceiling). Setting the ceiling equal to
  the base restores a flat cadence.

## Unreleased — web chat console (`ppz chat` for the browser)

**New GUI surface.** The ppz-server web UI gains a chat console at
`/orgs/<slug>/chat` — the browser port of the `ppz chat` TUI. Same three
roster sections (**Agents** = pty sources, **Inboxes** = message sources,
**Pipes** = uncollared rooms); selecting a row opens a live chat pane and a
composer. A "Chat" link sits alongside the org page's Pipes/Users/API-keys
tabs.

- **One stream per window.** The server has direct JetStream access, so a
  window is a straight follow of its stream — agent/inbox windows follow
  `<handle>.inbox`, pipe windows follow the uncollared pipe. Our own posts
  echo back through the follow, so there's no optimistic-echo/rollback dance
  the TUI needs; JetStream is the durable record.
- **Live tail over WebSocket.** `GET …/chat/ws?kind=&target=` replays a
  window's retained history then follows new publishes as JSON frames — the
  browser's single source for both, so the backlog crosses the wire once.
  Session-gated **and membership-gated** (unlike the read-only terminal WS).
- **Send from the browser.** `POST …/chat/send` publishes with the viewer's
  username as the envelope sender; messages the viewer sent render as `you`.
- **Cross-tenant guard.** All chat routes require org membership (not just a
  valid session), so a send can't inject into another org's streams; a
  non-member gets 404.
- **Agent liveness** dots (online / stale / offline) are read from each pty
  source's `heartbeat` stream using the same thresholds as `ppz who`.
- Snapshot endpoint `GET …/chat/messages?kind=&target=` returns a window's
  buffered history as JSON (for scripts / non-browser clients).
- **Bounded history.** Both the WS replay and the snapshot deliver at most the
  most-recent 200 messages (tail-N), so opening a busy window can't dump an
  unbounded backlog — one GetMsg round-trip per message — the same degeneracy
  the CLI read-flood cap guards against. (Older scrollback awaits the
  pagination cursor noted below.)
- *Not yet:* per-window unread badges (needs a per-user server-side read
  cursor); DM reply-fanout (the console shows a target's inbox directly rather
  than reconstructing a participant's stitched view); addressing a source under
  a non-root manifold (the console keys sources by bare handle, matching
  `ppz source create HANDLE` and the TUI's handle-keyed DMs).

## Unreleased — read flood cap (head-N paging)

**Behaviour change (CLI).** `ppz read` and `ppz subs read` now deliver at most
the **next 10 unread** messages per invocation (per pipe for `subs read`),
oldest first. Flood protection for agent consumers: an unbounded drain of a
spammed pipe could dump the whole retained backlog (up to 5000 msgs / 16 MiB)
into the reader's context, and in `subs read` one noisy pipe starved every
pipe sorted after it.

- **Head-N, not tail-N.** The cap takes the *oldest* N unread and advances the
  session cursor only past what was delivered — nothing is skipped; repeated
  invocations page through the backlog in order. (`reread -l` keeps its
  tail-N replay semantics; `read`'s `-l` pages forward.)
- **`-l N` overrides the cap; `-l 0` restores the unbounded drain.** On
  `subs read` the cap applies per pipe. `-l` is mutually exclusive with
  `--tail`, which still streams everything live.
- **`--limit` is the long form of `-l`** on `read`, `reread`, and
  `subs read` — identical semantics per verb (head-N paging on the read
  verbs, tail-N replay on `reread`).
- **Truncation is loud.** A capped read ends with a
  `(N more unread - run again to continue)` trailer. Suppressed under
  `--raw` / `--json` / `--bare`, which promise script-stable output.
- Wire: `ReadRequest.head_limit` (new, 0 = uncapped) and a trailing
  `ReadEvent.more_unread` event. Older CLIs against a newer daemon are
  unaffected (no head_limit → uncapped, no trailer emitted).

## Unreleased — remove `ppz terminal create`

**Breaking (CLI surface).** Removed the `ppz terminal create HANDLE` subverb.
It only provisioned a pty-shaped source and set it current — it never ran a
process, so a freshly "created" terminal produced no `stdout` stream and no
heartbeats, which read as broken. The pty pipe set has no meaning until
something runs in it. Use instead:

- **`ppz source create HANDLE`** — claim a handle and set it current
  (message-kind, `inbox` auto-pipe). The `E_NO_CURRENT_SOURCE` recovery hint
  now points here.
- **`ppz terminal share HANDLE [-- CMD]`** — run a live pty bound to `HANDLE`
  (auto-creates the handle on first use), publishing `HANDLE.stdout`, reading
  `HANDLE.stdin`, and emitting heartbeats. This is what actually produces a
  streaming terminal.

`agent create` is unaffected — it already routed through `terminal share`, not
`terminal create`.

## v0.31.1 — Strict bare rule + first-wins collisions (Phase 1.5.1)

**Breaking release.** Wire-level stream naming changed — cutover via Reset Database action then redeploy.

Tightens the four-role data model shipped in v0.31.0. Locks the design questions left open at v0.31.0: how bare names resolve at create-time, how collisions are reconciled across the source/pipe/manifold namespaces, and what `ppz send LEAF` does when LEAF could mean an uncollared pipe or a source's `.inbox`. Also fixes the v0.31.0 regression where `ppz send LEAF` failed with `E_SOURCE_NOT_FOUND` for uncollared pipes (reported on pipescloud.io).

### New

- **Strict bare-name rule.** `ppz pipe create LEAF` and `ppz pipe destroy LEAF` no longer auto-collar under the current handle. Bare names always resolve to an uncollared pipe at the current namespace. To create a collared pipe you must say so explicitly: `ppz pipe create <source>.<pipe>`. Resolves an ambiguity where `set namespace X` + `set handle Y` + `pipe create Z` had two equally plausible interpretations.
- **First-wins collision rule.** Within a manifold, source-handles and uncollared-pipe-names share a single namespace — you cannot create a resource that conflicts with an existing one. New error code **`E_NAME_TAKEN`** (exit 21) with three constructor forms:
  - `name 'X' is already taken by source at <root|manifold M>`
  - `name 'X' is already taken by uncollared pipe at <root|manifold M>`
  - `manifold path 'M.X' is reserved by source 'X' at <root|manifold M>`
- **Manifold-prefix reservation.** A source `X` at manifold `M` reserves the manifold path `M.X` because its auto-pipes (`inbox`/`stdin`/`stdout`/`stdctrl`) already publish at those subjects. Creating an uncollared pipe at `M.X` (or any deeper sub-path) is rejected — would otherwise collide on the wire.
- **Send shorthand fallback.** `ppz send LEAF "msg"` now tries the uncollared pipe `LEAF` first and falls back to the source shorthand `LEAF.inbox` if `LEAF` is a source. With the collision rule preventing both shapes from coexisting at the same manifold, the fallback is unambiguous. Fixes the v0.31.0 regression.
- **Namespace-aware source creation.** `ppz set namespace M` then `ppz source create X` creates the source at manifold `M` (was: always root). The session's `current_namespace` and `current_handle` are independent slots.
- **`E_PIPE_TAKEN` for uncollared pipes** now renders `uncollared pipe 'X' already exists at <root|manifold M>` instead of the collared `on source X` form (which made no sense for sourceless pipes).

### Wire-level changes

- **JetStream stream naming format changed**: `source_<orgshort>_<handle>_<pipe>` → `pipe_<orgshort>[_<manifold>][_<source>]_<name>`. All existing streams under the old name are orphaned (the new code neither reads nor writes them). Subject grammar is unchanged for root-collared shape — only the stream container name moved.
- 17 server + daemon callsites threaded through `natsubj.BuildSubject` / `natsubj.BuildStreamName` (replacing the pre-Phase-1.5 three-role `Subject` / `StreamName`).

### Cutover

Same sequence as v0.31.0:

1. Reset Database action — drops + recreates the production DB, leaves ppz-server stopped. Also clears the orphaned JetStream streams.
2. Deploy v0.31.1 — `systemctl restart` brings up the new binary against the empty DB; baseline 0001 + 0002 migrations run cleanly.
3. Smoke-test the live deployment.

## v0.31.0 — Data model under the new CLI surface (Phase 1.5)

**Breaking release.** Pre-launch schema bump — cutover via Reset Database action then redeploy.

Adds the structural primitives Phase 1's CLI surface implied but didn't ship: explicit hierarchical-grouping (manifold) on sources and pipes, sourceless (uncollared) pipes for symmetric many-to-many channels, and the namespace daemon-state verb that lets users scope subsequent pipe creates into a manifold.

### New

- **`manifold` column** on `sources` and `pipes` (text, NOT NULL DEFAULT `''`). Empty string represents the root namespace. Multi-team self-hosters and pipescloud use non-empty values; OSS-default deploys leave everything at `''`.
- **Sourceless (uncollared) pipes** — `pipes.source_id` is nullable. `ppz pipe create LEAF` with no current handle creates an uncollared pipe; symmetric many-to-many semantics. Wire form: `<account>.<manifold?>.<pipe>` (no source segment).
- **`ppz set namespace PATH`** / **`ppz unset namespace`** — daemon-state verbs that scope subsequent pipe creates into the given manifold. View via `ppz status` (no `ppz get namespace` — status is the read interface).
- **`POST /api/v1/pipes`** — new HTTP endpoint for full-path-aware pipe creation. Body shape adds `manifold` and nullable `source_handle`. The pre-Phase-1.5 collared-shortcut `POST /api/v1/sources/{handle}/pipes` stays as-is.
- **`natsubj.BuildSubject`** and **`natsubj.BuildStreamName`** — four-role helpers per locked decision #18.

### Wire grammar (locked decision #18)

```
<account>.<manifold?>.<source?>.<pipe>
```

Wire-level the manifold-only and source-only shapes are indistinguishable — disambiguation happens by DB row at create time, not by the broker. See `docs/WIRE.md` §1.

### ACL

The existing per-account wildcard `<accountID>.>` already covers uncollared pipes by pattern match — no JWT-mint changes were required. Leaf-name conventions (`inbox` subscribe-only, `stdout` publish-only, etc.) and role-asymmetry inference are deferred to **Phase 3**.

### Cutover

Pre-launch schema bump. Same sequence as v0.30.2:

1. Reset Database action — drops + recreates the production DB, leaves ppz-server stopped.
2. Deploy v0.31.0 — `systemctl restart` brings up the new binary against the empty DB; baseline 0001 + 0002 migrations run cleanly.
3. Smoke-test the live deployment.

## v0.30.0 — Pre-launch surface strip (Phase 1)

**Breaking release.** Removes three concepts from the user-facing CLI
before launch — they were OSS pre-release surface that didn't survive
field-signal review or didn't match how teams use the tool. Pipescloud
will layer org/team/project management above the OSS account primitive
in its closed-source control plane.

### Removed

- **`ppz org`** — `ppz org list/switch/create/invite` are gone. Multi-org
  tenancy moves to pipescloud's control plane; OSS keeps single-tenant
  accounts as the default deployment shape. The HTTP endpoints
  `GET /api/v1/orgs` and `POST /api/v1/orgs` are also removed.
- **`ppz broadcast`** — both the CLI verb and the `<handle>.broadcast`
  auto-provisioned pipe are gone. Teams overwhelmingly use shared "room"
  pipes (e.g. `ppz pipe create team1.room` with implicit `--writers=anyone`),
  not one-to-many announce.
- **`ppz source switch / clear`** — gone (cleanly replaced; see migration
  table below). `ppz source create` and `ppz source destroy` *survive*
  the strip — their semantics aren't covered by other verbs.

### Renamed (schema + Go types)

- `organisations` table → `accounts`
- `organisation_members` table → `account_members`
- `organisation_id` columns → `account_id` (api_keys, sources, invites,
  account_members)
- `db.Organisation` Go type → `db.Account`; methods follow
  (`InsertAccount`, `ListAccounts`, etc.)
- `OrganisationID` Go fields and `OrgID`/`OrgName` JSON fields →
  `AccountID` / `account_id` / `account_name` everywhere (`StatusReply`,
  `LoginReply`, `AuthExchangeRequest`, `Credentials`, `Invite`).

### New

- **`ppz set [key] [value]`** — daemon-state CLI pattern. Day-one keys:
  `handle`.
  - `ppz set handle HANDLE` switches the daemon's current handle
    (replaces `ppz source switch HANDLE`).
- **`ppz unset [key]`** — clears state.
  - `ppz unset handle` (replaces `ppz source clear`).
- **`ppz get [key]`** — reads state. Single-line stdout; exits 1 if
  empty so `$(ppz get handle) || handle=` is scriptable.
- **`ppz pipe destroy --recursive HANDLE`** — bulk destroys every pipe
  under a handle, plus the handle row itself. Replaces
  `ppz source destroy HANDLE`.
- **`ppz terminal create HANDLE`** — provisions a pty-kind handle
  (inbox + stdin/stdout/stdctrl pipes) and sets it as current. Direct
  replacement for `ppz source create HANDLE` when you want the full
  pty-style pipe set. (`ppz agent create HANDLE` already existed since
  v0.29.)

### Migration

- **Schema is destructive**: the `organisations` → `accounts` rename
  cannot be applied to existing pre-launch installs as a no-op.
  Self-hosters on v0.29 or earlier must **drop and reinitialise the
  database**. Pre-launch with no production users this is acceptable.
- **CLI verb replacements**: at-a-glance migration table—

  | Pre-Phase 1 (v0.29) | Post-Phase 1 (v0.30) |
  |---|---|
  | `ppz org list/switch/create/invite` | (web UI — pipescloud only) |
  | `ppz broadcast HANDLE MSG` | `ppz pipe create HANDLE.room` once, then `ppz send HANDLE.room MSG` |
  | `ppz source create HANDLE` | unchanged — `ppz source create HANDLE` (bare actor identity; auto-pipe set is just inbox). For richer pipe bundles, use `ppz terminal create HANDLE` (pty) or `ppz agent create HANDLE` (agent harness). |
  | `ppz source switch HANDLE` | `ppz set handle HANDLE` |
  | `ppz source clear` | `ppz unset handle` |
  | `ppz source destroy PATTERN` | unchanged — `ppz source destroy PATTERN` (glob across handles and pipes). For per-handle recursive destroy, `ppz pipe destroy --recursive HANDLE` also works. |

### Internal / not user-visible

- IPC verb constants `IPCBroadcast` / `IPCBroadcastBatch` retained as
  the publish-IPC path (`ppz send`, `ppz command`, terminal stdin
  forwarding still use them); commented as such.
- IPC verb constants `IPCSwitch` / `IPCDisconnect` / `IPCSourceDestroy`
  retained as the daemon-state mutation path; `ppz set handle` /
  `ppz unset handle` / `ppz pipe destroy --recursive` route through
  them.
- `db.Source` Go type, `sources` table, and `db.Source.Pipes()` /
  `IsAutoPipe()` retained — terminal/agent create still go through
  them. Pipes table's `LastBroadcastAt` / `LastBroadcastPayload`
  columns dead but harmless until the schema fully collapses.
- The "drop sources table; subject grammar collapses to
  `<account>.<path>`" architectural step is deferred to a follow-up
  release (would be Phase 1.5 or fall out of Phase 3 ACL work).
