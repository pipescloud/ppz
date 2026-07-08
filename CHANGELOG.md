# Changelog

## Unreleased — message priority

`ppz send --priority 1|high, 2|medium, 3|low` stamps a delivery-order hint
on the envelope (new always-serialised `priority` field; `0` = unset ≡
medium, so legacy messages and priority-free meshes behave byte-identically).
Recipients' inbox/broadcast drains deliver higher tiers first —
`sort.SliceStable`, so FIFO is preserved within a tier. Scope rules:

- Sorting applies to the retained batch of a single `read`/`reread`/
  `subs read` drain, AFTER `--skip`/`-l` trimming (those windows keep their
  arrival-order meaning). A reader draining one message at a time sees no
  reordering.
- `--tail` stays fully arrival-ordered (backlog + live) — one invocation,
  one ordering discipline. Byte-faithful pipes (stdout/stdin/stdctrl/custom)
  are never reordered.
- Cursor advance and `ack:read` emission are keyed off stream sequence —
  unaffected by delivery order. Auto-emitted acks carry priority 0.
- Tabular read prepends an advisory `P1 `/`P3 ` badge for explicit high/low
  only. The badge is forgeable by payload text; agents should trust the
  structured `priority` field (`--json`), never the inline text.
- Invalid values reject with the new `E_INVALID_PRIORITY` (exit 27) at the
  CLI and at the daemon IPC trust boundary; readers additionally clamp
  out-of-range wire values to medium.

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
