# ppz Wire Contracts

This document is **authoritative**. Implementation, tests, and clients must
match it byte-for-byte. If reality drifts, fix reality, not the doc — unless
the doc is being deliberately revised in the same commit alongside the
implementation and the test fixtures.

All timestamps are RFC3339 UTC (e.g. `2026-04-25T12:34:56Z`). All IDs are
UUID v7 unless noted. All JSON request/response bodies use
`application/json; charset=utf-8`.

## 0. Vocabulary

- **account** — the tenancy boundary. One ppz-server deployment may host
  several. Pre-launch this was called "organisation"; the rename is part of
  the Phase 1 surface strip (v0.30.0).
- **source** — the top-level addressable entity (formerly "pipe"). Each source
  has a unique `handle` within an account and a `kind` (`message` or `pty`).
- **handle** — the human-facing identifier of a source. Also the name for the
  daemon's per-session "current handle" state.
- **pipe** — a named sub-bucket on a source where messages flow. A `message`
  source has one pipe (`inbox`); a `pty` source has four (`inbox`, `stdin`,
  `stdout`, `stdctrl`). The `broadcast` auto-pipe was removed in v0.30.0
  (see CHANGELOG); custom pipes are created explicitly via `ppz pipe create`.

A target on the wire is `<source-handle>.<pipe-name>`.

## 1. Subject grammar (NATS)

Phase 1.5 adopts the four-role form (locked decision #18):

```
<account_id>.<manifold?>.<source?>.<pipe>
```

- `account_id` — UUID of the account (lowercase, hyphenated form). Hard tenancy boundary.
- `manifold` — **optional**, 0+ dot-separated segments. Hierarchical-grouping path. Empty (the bare `<account_id>.…` form) = root namespace. Each segment matches the handle regex.
- `source` — **optional**, 0 or 1 segment. Actor identity (the "collar"). Present = collared pipe (role-asymmetric semantics anchored on the source identity); absent = uncollared (symmetric many-to-many).
- `pipe` — pipe leaf. Built-in: `inbox`, `stdin` (pty only), `stdout` (pty only), `stdctrl` (pty only). Reserved: `system`, `db`.

Wire-level the manifold-only and source-only shapes are **indistinguishable** (`<acct>.X.<pipe>` could be either). That's by design — disambiguation happens by DB row at create time, not by the broker. Clients send unambiguous create requests with explicit `manifold` + `source_handle` fields; the broker just does prefix-based ACL.

Handle / source segment regex: `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`, max 32. Reserved handles (rejected at create): `system`, `db`.

Four wire shapes:
- `<acct>.<pipe>` — root manifold, uncollared
- `<acct>.<source>.<pipe>` — root manifold, collared
- `<acct>.<manifold-segments>.<pipe>` — namespaced, uncollared
- `<acct>.<manifold-segments>.<source>.<pipe>` — namespaced, collared

## 2. JetStream stream config (per pipe)

Stream names use the four-role builder (Phase 1.5). Dots in manifold segments become underscores (NATS forbids dots in stream names); empty manifold/source slots are omitted entirely.

| Field | Default |
|---|---|
| Name | `pipe_<orgshort>[_<manifold-underscored>][_<source>]_<name>` (orgshort = first 8 hex chars of account UUID, hyphens stripped). Pre-Phase-1.5 collared shortcut endpoint still emits `source_<orgshort>_<handle>_<pipe>` for back-compat. |
| Subjects | `<account_id>[.<manifold>][.<source>].<pipe>` per §1 |
| Retention | Limits |
| MaxAge | 24h |
| MaxMsgs | 5000 |
| MaxBytes | 16 777 216 (16 MiB) |
| Storage | File |
| Discard | Old |
| Replicas | 1 |

## 3. Envelope

Published payload on every `<org_id>.<handle>.<pipe>`:

```json
{
  "id": "<uuid v7>",
  "sender": "<source-handle-or-empty>",
  "subject": "<header-line-or-empty>",
  "payload": "<utf-8 string>",
  "created_at": "<rfc3339>",
  "in_reply_to": "<uuid-or-empty>",
  "ack_requested": false,
  "priority": 0
}
```

Constraints:
- `payload` is always a UTF-8 string. Binary data must be base64-encoded by the
  caller.
- Encoded envelope must not exceed 65 536 bytes after JSON marshalling. The
  daemon enforces this *before* publishing and returns `E_PAYLOAD_TOO_LARGE`.
- `sender` is the publisher's own current source at publish time — *who is
  speaking*, distinct from where the message is going (the destination is
  encoded only in the subject's `<handle>`). It is the empty string when
  the publishing session has no current source set (e.g. `ppz send <dest>`
  from a session whose current is unset).
- `subject` is an optional header-line (separate from the NATS subject in §1).
  Two roles: free-form text (rendered as `[subject] payload` in the read
  default for inbox-shaped pipes) and reserved system prefixes — strings
  starting with `ack:` are reserved for daemon-emitted protocol messages
  (e.g. `ack:read`) that the read formatter renders as
  `<subject> → <last-8-hex-of-id>`. The empty string means "no subject".
- `in_reply_to` (v0.25.0) is the UUID of the message this one replies to —
  set on `ack:read` envelopes by the daemon's auto-emit hook so the
  original sender can correlate the ack to a specific message. Empty
  string when the envelope isn't a reply.
- `ack_requested` (v0.25.0) is the sender's opt-in to a daemon-emitted
  read receipt: when the recipient's `ppz read` cursor advances past a
  message with `ack_requested: true`, the recipient's daemon publishes
  an `ack:read` envelope back to `<original_sender>.inbox`. The flag
  is **best-effort and non-blocking** — a failed ack publish does not
  block cursor advancement, so a sender sees no ack if either the
  recipient hasn't read yet OR the ack publish itself failed (the two
  are indistinguishable). Senders requiring strict guarantees should
  layer their own re-send-on-timeout pattern. The auto-emitted
  `ack:read` envelope carries `ack_requested: false` (loop guard).
- `priority` (v0.51.0) is the sender's delivery-order hint: `1`=high,
  `2`=medium, `3`=low. `0` means unset (the sender didn't pass
  `--priority`; also daemon-emitted acks and terminal-share batch
  frames) and readers treat it as medium. Senders publish only
  `{0,1,2,3}` — `ppz send` validates the flag and `handleSend` re-checks
  at the IPC trust boundary (`E_INVALID_PRIORITY`); readers additionally
  clamp any out-of-range value to medium at sort time, so an envelope
  written straight onto NATS with a garbage priority cannot mint a
  super-priority tier. Priority affects only how a recipient's drain
  *orders* messages (§8 `ppz read`); it never affects storage, cursors,
  or ack emission.
- All envelope fields are **always serialised**, even when empty / false,
  so receivers see a stable wire shape per release. Marshalling does
  NOT use `omitempty` for any of `sender` / `subject` / `in_reply_to` /
  `ack_requested` / `priority`.
- Pre-v0.23.0 envelopes carried a `handle` field equal to the destination
  and no `sender` / `subject`. Pre-v0.25.0 envelopes additionally lack
  `in_reply_to` / `ack_requested`. Pre-v0.51.0 envelopes lack
  `priority` (zero-valued on decode = unset ≡ medium). Decoders MUST silently drop unknown
  fields (`encoding/json` does this by default — do not opt into
  `DisallowUnknownFields` on envelope payloads), so retained legacy
  messages parse cleanly under the new shape with the missing fields
  zero-valued. They age out of JetStream within 24h (per §2 MaxAge).

## 4. NATS auth

- Single account `PPZ` on the embedded NATS server.
- Server signs short-lived user JWTs scoped to a single org.
- JWT permissions: `pub.allow=["<org_id>.>"]`, `sub.allow=["<org_id>.>"]`.
- TTL: 1 hour. Daemon refreshes at `exp − 5 min`.

## 5. HTTP API (`/api/v1`)

Auth: `Authorization: Bearer <api_key_plaintext>` on every endpoint **except**
the GUI HTML routes (which are unauthenticated).

`/auth/exchange` is the only endpoint where the API key is sent in the body
rather than the header (this allows the daemon to pre-validate the key during
`ppz login` before storing it). All subsequent calls use the header.

Error response shape (any non-2xx):
```json
{"error": {"code": "E_*", "message": "<human readable>"}}
```

### 5.1 POST /api/v1/auth/exchange
Body: `{"api_key": "<plaintext>"}`

200:
```json
{
  "jwt": "<nats user jwt>",
  "nats_url": "nats://<host>:4222",
  "org_id": "<uuid>",
  "expires_at": "<rfc3339>"
}
```

Errors: 401 `E_INVALID_API_KEY`.

### 5.2 POST /api/v1/sources
Body: `{"handle": "foo", "kind": "message" | "pty"}`

201:
```json
{
  "id": "<uuid>",
  "handle": "foo",
  "kind": "message",
  "created_at": "<rfc3339>"
}
```

Errors: 401, 400 `E_INVALID_HANDLE`, 409 `E_SOURCE_TAKEN`.

### 5.3 GET /api/v1/sources

200 (sorted by handle ASC):
```json
{
  "sources": [
    {
      "handle": "alpha",
      "kind": "message",
      "last_broadcast_at": "2026-04-25T12:34:56Z",
      "last_broadcast_payload": "hello"
    },
    {
      "handle": "beta",
      "kind": "pty",
      "last_broadcast_at": null,
      "last_broadcast_payload": null
    }
  ]
}
```

Errors: 401.

### 5.4 GET /api/v1/sources/{handle}
200: same shape as a single element of `/api/v1/sources`.
Errors: 401, 404 `E_SOURCE_NOT_FOUND`.

## 6. Server GUI (HTML, unauthenticated)

| Method | Path | Behaviour |
|---|---|---|
| GET | `/` | Lists orgs (id, name, created_at). Form posts to `/orgs`. |
| POST | `/orgs` | Form field `name`. Creates org. Redirects to `/orgs/{slug}`. |
| GET | `/orgs/{slug}` | Shows api keys (label, prefix, created_at) and sources as a table: handle, pipe, last_broadcast_at, payload (truncated to 60 chars). Pipe cell links to the pipe detail page. Form posts to `/orgs/{slug}/keys`. |
| POST | `/orgs/{slug}/keys` | Form field `label`. Creates api key. Renders the **plaintext key once** on the response page. Subsequent visits to `/orgs/{slug}` show only the prefix. |
| GET | `/orgs/{slug}/sources/{handle}/pipes/{pipe}` | Lists every buffered message on this pipe from the JetStream stream, in chronological order. Honors stream retention (defaults: 24 h / 1000 msgs / 64 MiB, whichever first). |

All HTML pages are server-rendered (Go `html/template`). No JS framework.

### 6.1 Stable HTML markers (for tests)

Tests scrape these via `grep -oP 'data-…="\K[^"]+'`. They are part of the
contract — do not rename:

| Page | Marker | Value |
|---|---|---|
| `GET /` | `data-org="<name>"` | one per org listed |
| `GET /orgs/{slug}` | `data-key-prefix="<prefix>"` | one per api key |
| `GET /orgs/{slug}` | `data-source="<handle>"` | one per source row in the table |
| `GET /orgs/{slug}` | `data-source-row="<handle>:<pipe>:<rfc3339-or-empty>:<payload60-or-empty>"` | one per (source, pipe) row |
| `GET /orgs/{slug}` | `data-source-pipe-link="/orgs/<slug>/sources/<handle>/pipes/<pipe>"` | one per pipe cell |
| `GET /orgs/{slug}/sources/{handle}/pipes/{pipe}` | `data-message="<id>:<rfc3339>:<payload>"` | one per buffered message, chronological |
| `POST /orgs/{slug}/keys` (response page) | `data-new-key="<plaintext>"` | exactly one |

## 7. CLI ↔ daemon IPC

Unix domain socket at `$PPZ_IPC_SOCKET` (default `$PPZ_HOME/daemon.sock`,
which itself defaults to `~/.ppz/daemon.sock`).

Wire format: newline-delimited JSON-RPC 2.0. One request per connection, the
daemon writes one response and closes (simple half-duplex; long-running reads
keep the connection open and stream `ReadEvent` lines until the client closes).

Methods (Phase 1.5 reality — verbs unchanged in this phase):

| Method | Params | Result |
|---|---|---|
| `Status` | `{}` | `{"daemon_pid":int,"daemon_version":str?,"logged_in":bool,"url":str?,"key_prefix":str?,"org_id":str?,"current":str?,"nats_state":str?}` |
| `Login` | `{"url":str,"api_key":str}` | `{"url":str,"key_prefix":str,"org_id":str}` |
| `Create` | `{"handle":str,"kind":str}` | `{"handle":str,"subject":str,"kind":str}` |
| `Switch` | `{"handle":str}` | `{"handle":str}` |
| `Broadcast` | `{"handle":str?,"channel":str?,"payload":str}` | `{"id":str,"subject":str,"bytes":int}` |
| `List` | `{"session":str?}` | `{"sources":[{"handle":str,"kind":str,"pipe_infos":[{"pipe":str,"total":int,"unread":int,"last_at":str?,"preview":str},…],"last_broadcast_at":str?,"last_broadcast_payload":str?},…]}` |
| `Read` | `{"handle":str,"channel":str,"limit":int?,"skip":int?,"since_ms":int?,"json":bool?,"follow":bool?,"session":str?,"no_advance":bool?}` | streaming `ReadEvent` JSON lines |
| `Diag` | `{}` | `{"nats_state":str?,"nats_drops_last_hour":int?,"nats_events":[{"type":str,"at":str,"reason":str?},…]}` |

Errors are returned as JSON-RPC errors with `code` = the integer exit code from
ERRORS.md and `message` = `"E_FOO: human readable"`.

(Note: legacy field names `channel` on `Broadcast`/`Read` requests still carry
the pipe name; this is preserved for IPC backward-compat within the Phase A
rename. Phase B reorganises these.)

## 8. Pinned stdout (CLI)

The harness diffs stdout byte-for-byte after normalisation
(`tests/lib/normalize.sh`). The exact bytes are the contract.

### `ppz daemon`
First invocation: `daemon started pid=PID`
Already running:  `daemon already running pid=PID`
Foreground (`--foreground`): same first line, then blocks.

### `ppz status`
Logged in with a current source:
```
daemon: logged in (pid=PID), <daemon_version> (<state>)
last token refresh: <relative time|->
server: <URL>
org: <org_name_or_id>
nats: <connected|disconnected|connecting|unknown> [(N <unit> ago)]
current source: <handle>
```
`<state>` is one of three values (since v0.31.9):
  - `latest` (green) — daemon binary matches the CLI binary AND no
    newer release is on the update manifest.
  - `update available, run 'ppz upgrade'` (amber) — daemon matches the
    CLI but the manifest advertises a newer release.
  - `daemon out of sync with ppz cli, run 'ppz daemon restart'` (red) —
    daemon binary disagrees with the CLI (typically right after `ppz
    upgrade` ran but the old daemon is still resident). Out-of-sync
    trumps update-available: restart first, upgrade after.

Logged in, no current source: same with `current source: -`.
Logged in, no auth: `daemon: not logged in (pid=PID), <daemon_version> (<state>)` plus a login hint.
Daemon not running (exit 11): `daemon: not running`.

The `nats:` line surfaces the daemon's current connection state to the
NATS server. The state vocabulary is fixed (`connected` /
`disconnected` / `connecting` / `unknown`). The line is deliberately
terse — per-event detail (drop counts, timestamps, error reasons)
lives in `ppz diagnostics` instead, so a noisy connection history doesn't
churn `ppz status` output.

A `(N <unit> ago)` suffix may follow the state token when the daemon
can anchor the current state to a transition event in its in-memory
ring. Absence of the suffix means the ring has no matching event
(fresh daemon, or the transition aged out) — the prefix `nats: <state>`
is the only part of the line consumers should depend on. The state
token colours encode stability (since v0.37.4): a clean first-connect
or a reconnect that has held for ≥1 minute is green; a reconnect under
1 minute old is amber (signalling a recent flap that may recur);
disconnected / unknown stay red.

### `ppz login URL -apikey K`
```
logged in url=<URL> key=KEYPREFIX org=<org_id>
```

### `ppz source create HANDLE` — one line
```
created handle=<handle> subject=<account_id>.<handle>.inbox
```

### `ppz set handle HANDLE`
```
handle=<handle>
```

### `ppz send HANDLE[.PIPE] "PAYLOAD" [--subject S] [--in-reply-to ID] [--request-ack] [--priority P]`

Bare handle defaults to `<handle>.inbox`. Success line goes to **stderr**
(not stdout) since v0.25.0 — scripts redirecting stdout previously
swallowed it. The `id` shown is the last 8 hex characters of the UUID
(visual brevity); the full UUID stays in the message envelope:

```
sent id=<id8> to=<handle>.<pipe> bytes=<n>
sent id=<id8> to=<handle>.<pipe> bytes=<n> ack=requested    # with --request-ack
```

Stderr only survives stdout-only redirects (`>file`, `>/dev/null`). It
does NOT survive combined-stream redirects (`&>file`, `2>&1 >file`) —
which is the explicit semantics of "redirect everything".

Flags (v0.25.0):
- `--subject S` — sets the envelope-level subject (header-line). The
  `ack:` prefix is reserved for daemon-emitted protocol messages and
  rejected by the CLI argument parser AND by the daemon's IPC trust
  boundary (`E_INVALID_SUBJECT`).
- `--in-reply-to ID` — sets the envelope's `in_reply_to` to a previous
  message's UUID; renders as a thread linkage in the tabular read
  default.
- `--request-ack` — requests a daemon-emitted `ack:read` back to the
  sender's inbox when the recipient's `ppz read` advances past this
  message. **Best-effort, non-blocking.** Requires a non-empty current
  source (preflighted at the CLI; emits `E_NO_CURRENT_SOURCE` if absent).

Flag (v0.51.0):
- `--priority P` — delivery-order hint: `1|high`, `2|medium|med`,
  `3|low` (aliases case-insensitive). Omitted = unset (`priority: 0`
  on the wire, treated as medium by readers). Any other value is
  rejected with `E_INVALID_PRIORITY` at the CLI AND at the daemon's
  IPC trust boundary. Only affects how the recipient's inbox/broadcast
  drain orders messages (see `ppz read` below); inert on byte-faithful
  pipes (stdout/stdin/stdctrl/custom).

The `--request-ack` flag triggers a read receipt — distinct from the
delivery acknowledgment the success line itself already provides. The
success line is written *after* the daemon's NATS PubAck confirms the
broker durably stored the message; `--request-ack` is asking
specifically for read confirmation.

### `ppz read HANDLE.PIPE [--tail --json --tty --raw --bare]`
Default depends on the pipe (since v0.23.0):
- `<handle>.inbox` and `<handle>.broadcast` → tabular three-column rows:
  ```
    HH:MM:SS  <sender|->  <body>
                          <continuation lines, indented under <body>>
  ```
  `<body>` is `[subject] payload` when subject is non-empty and not `ack:*`,
  or `<subject> → <last-8-hex-of-id>` for `ack:*` system subjects, or
  `payload` when no subject. Explicit high/low priorities prepend an
  advisory `P1 ` / `P3 ` badge to `<body>` (never for `ack:*` rows;
  unset/medium rows are byte-identical to pre-v0.51.0 output). The badge
  shares the text column with sender-controlled payload and is therefore
  forgeable — agents must trust the structured `priority` field
  (`--json`) or delivery order, never the inline text.
- All other pipes (stdout / stdin / stdctrl / user-named custom) → bare
  `evt.Message.Payload` followed by `\n` per message (byte-faithful).

Ordering (v0.51.0): for inbox/broadcast-shaped reads (including
uncollared pipes, which share the tabular default), the drained batch is
delivered **priority-first** — `EffectivePriority` ascending (1 high →
3 low; unset/garbage clamp to medium), stable within a tier so FIFO
stream-sequence order is preserved. A mesh where nobody sets a priority
is byte-identical to pre-v0.51.0 behaviour. Semantics and limits:
- The sort happens AFTER `--skip`/`-l` trimming — those windows keep
  their arrival-order meaning; priority governs order *within* the
  delivered window only.
- Priority reorders unread messages **within a single read drain**. A
  recipient reading one message at a time (e.g. the `subs wait` →
  `read` wake-per-message loop) accumulates no backlog and sees no
  reordering — priority helps a *busy* reader who drains several
  messages at once.
- `--tail` is fully arrival-ordered — backlog AND live. Live messages
  stream one-at-a-time and cannot be reordered, so the drained backlog
  is deliberately left unsorted too: one invocation, one ordering
  discipline.
- Byte-faithful pipes (stdout / stdin / stdctrl / custom) are NEVER
  reordered, even if a sender stamped a priority on a frame.
- Cursor advance and `ack:read` emission key off JetStream stream
  sequence, not delivery order — priority never changes which messages
  count as read or acked.

Flags:
- `--bare` forces the legacy payload-only output for any pipe — script-stable
  opt-out from the new tabular default. Mutually exclusive with `--json`,
  `--tty`, `--raw`.
- `--json` prints the full envelope JSON, one per line.
- `--tty` / `--raw` unchanged — see cmdRead doc.

`--bare` / `--raw` / `--json` are *output-format* flags, not *ordering*
flags: on an inbox/broadcast read they select how each message is
rendered but the batch is still priority-ordered (§ordering above). Only
the pipe shape (byte-faithful pipes) and `--tail` suppress reordering —
so `ppz read inbox --bare` still delivers high-priority messages first.
Reach for a byte-faithful pipe, not `--bare`, when you need strict
arrival order on a message pipe.

(`reread` mirrors `read`'s output flags including `--bare`.)

### `ppz diagnostics [--json]`

Daemon introspection (Phase 0 of agent hardening). Prints the current
NATS connection state plus the most recent connection-state events the
daemon has observed (capped at 32 entries, drop-oldest) along with the
last few daemon lifecycle events (`daemon_start` / `daemon_stop`).
Useful for catching transient outages or daemon bounces "a few minutes
ago" that have already recovered by the time `ppz status` runs.

The verb deliberately does NOT require login. An operator hitting a
sick daemon (login fails, NATS unreachable) needs `ppz diagnostics` to
work — that's the whole point. Only `ppz status` reporting "daemon: not
running" prevents `ppz diagnostics` from succeeding (no socket to
talk to).

Lifecycle events (`daemon_start` / `daemon_stop`) persist across
daemon restarts via a tiny on-disk log under `$PPZ_HOME` — a fresh
daemon's in-memory ring is otherwise empty, so without persistence
"the previous daemon stopped at HH:MM:SS" would vanish the instant
the next daemon comes up. The persisted log is trimmed to the most
recent 32 lifecycle entries on each append.

Default output:
```
nats: <connected|disconnected|connecting|unknown> drops_last_hour=N events=N
<type> <RFC3339-timestamp> reason="<text>"
…
```

Where `<type>` is one of `disconnect` / `reconnect` / `closed` /
`daemon_start` / `daemon_stop`. The `reason` field captures the
underlying error string for disconnect / closed events (e.g.
`"connection closed"`); for reconnect it captures the URL the client
reconnected to. Empty when nats.go provided none.

Test contract: each event line begins with the type token (anchored
to start-of-line). Detail tokens after the timestamp are free-form —
add fields as Phase 1+ work surfaces them, but don't change the type
prefix.

`--json` emits a single JSON object matching the IPC `DiagReply`
shape: `{"nats_state":str, "nats_drops_last_hour":int,
"nats_events":[{"type":str, "at":str, "reason":str}, …]}`.

### `ppz kill`
`daemon stopped pid=PID` if running, `daemon not running` if not. Exit 0 either way.

### `ppz daemon restart`
Runs `ppz daemon stop` followed by `ppz daemon start` — two output
lines, both at exit 0:
```
daemon stopped pid=PID
daemon started pid=PID
```
When no daemon was running, the first line is `daemon not running`
instead. The verb exists so the red-state `ppz status` daemon line
("daemon out of sync with ppz cli, run 'ppz daemon restart'") has a
single command behind it.

### `ppz ls`
One line per (source, pipe), sorted by `<handle>.<pipe>` ASC. Fields separated
by single spaces. Missing values `-`. Preview is the most recent payload
truncated to 60 chars (UTF-8 safe), with ANSI CSI sequences and C0 controls
stripped.

```
<namespace|-> <handle>.<pipe> <total> <unread> <last_at|-> <preview60|-> <creator>
```

Columns rendered (header form): `NAMESPACE  PIPE  UNREAD  BUFFERED  LAST  PAYLOAD  CREATOR`.

`<namespace>` is the manifold the pipe lives in. Root namespace renders as
`-` (the same missing-value glyph used by LAST and PAYLOAD); a non-root
manifold renders as the dot-separated path verbatim (e.g. `team-a` or
`team-a.subteam`). The PIPE column carries only `<handle>.<pipe>` (or just
`<pipe>` for uncollared) — the manifold prefix moves out of PIPE and into
NAMESPACE so callers can address the two facts independently.

`<creator>` is the username that created the (source, pipe). Per-pipe attribution
falls back to the source's creator when the pipe row carries no creator of its
own (i.e. for the auto-provisioned `broadcast` / `inbox` / `stdin` / `stdout` /
`stdctrl` pipes, which have no row in the `pipes` table). The seeded API keys
attribute deterministically: `alpha-primary→foo`, `alpha-secondary→bar`,
`beta-primary→bar`.

`ppz ls --json` emits one JSON object per row with the keys `{namespace,
handle, pipe, total, unread, last_at, payload, creator}` (full untruncated
payload, ISO timestamp). The `namespace` key carries the same manifold the
table renders (empty string for root). The `creator` key carries the same
username the table renders.

Empty list: zero output, exit 0.

### `ppz terminal share HANDLE [-- CMD ...]`
Wraps a shell (or `<cmd>`) in a PTY bound to source `HANDLE` (kind=pty;
auto-creates the source on first use), with `PPZ_CURRENT_HANDLE=<handle>`
exported to the child. Stdout chunks publish verbatim to `<handle>.stdout`;
subscribed `<handle>.stdin` messages forward to the PTY master. Foreground;
blocks until child exits. Exit 0 on clean child exit.

### `ppz terminal watch HANDLE`
TUI viewer: enters alt-screen, follows `<handle>.stdout` until SIGINT/Ctrl-C,
exits alt-screen, exit 0.

## 9. Daemon on-disk state

Under `$PPZ_HOME` (default `~/.ppz`):

| File | Contents |
|---|---|
| `daemon.pid` | The daemon process PID, written at startup. |
| `daemon.sock` | The unix-domain IPC socket. |
| `credentials` | JSON `{"url":"…","api_key":"…"}`, mode 0600. Absent ⇒ not logged in. |
| `current` | Plain text: the current source handle, no trailing newline. Absent ⇒ no current source. |
| `cursors/<session>.json` | Per-session map `{"<orgID>.<handle>.<pipe>": <stream_seq>, …}` — highest delivered JetStream sequence per pipe per session. Used for unread counts in `ls` and to resume `read`. Session id resolves from `$PPZ_SESSION` → `tty(1)` → `"default"`. |

The daemon does NOT cache cursor state in memory across calls — every Get/Advance
re-reads/writes the file. (The harness wipes `cursors/` between scenarios; an
in-memory cache would mask that wipe and cause false-negative unread counts.)

## 10. Test-only knobs

| Env var | Purpose |
|---|---|
| `PPZ_TEST_CLOCK` | If set to an RFC3339 timestamp, server- and daemon-issued `created_at` fields use this value (frozen clock). Honoured by `internal/clock`. |
| `PPZ_IPC_SOCKET` | Override unix socket path (test-runner uses two: `/tmp/a/daemon.sock`, `/tmp/b/daemon.sock`). |
| `PPZ_HOME` | Override credential storage dir (default `~/.ppz`). |
| `PPZ_SESSION` | Override session id used to key cursor state (otherwise derived from `tty(1)`, falling back to `"default"`). |
| `PPZ_TEST_FILTER` | Glob filter for `tests/run.sh`. |
| `PPZ_NATS_URL` | Override the NATS URL the daemon dials, regardless of what `/auth/exchange` returned. Useful when running daemon outside compose against an in-compose NATS. |
| `PPZ_CURRENT_HANDLE` | Override the daemon's `current` for one CLI invocation (exported by `terminal share` / `agent create` into the wrapped child so its `ppz` calls resolve the wrap's handle as current). |

## 11. Heartbeat (`<handle>.heartbeat`)

`ppz terminal share` (and therefore `ppz agent`) publishes one JSON object
per beat to the `<handle>.heartbeat` pipe: every `interval_sec` (60s), plus
an immediate out-of-cycle beat whenever the detected agent state transitions
(working↔idle, harness appeared/exited). `ppz who` reads the latest beat per
handle.

All keys are always serialized — predictable wire shape, zero values
included. Changes are additive: consumers ignore unknown keys, renderers
show `-` for missing ones. (Source of truth: `HeartbeatPayload`,
`internal/cli/heartbeat.go`.)

| Key | Meaning |
|---|---|
| `ts` | Beat time, RFC 3339 UTC. |
| `seq` | Per-process monotonic counter, starts at 1; wake-driven and tick-driven beats share it. |
| `harness` | Canonical harness id (`claude` / `codex` / `copilot` / `agy` / `pi`) or `""`. Live PTY foreground detection wins over the launch-time `PPZ_AGENT_HARNESS` env var (detection tracks the actual foreground; the env var survives the harness exiting back to a shell). |
| `harness_source` | Which side won: `"detected"` (live foreground detection), `"env"` (`PPZ_AGENT_HARNESS` fallback), `""` (neither). |
| `agent_state` | `""` / `"idle"` / `"working"` (`"blocked"` reserved for detection phase 3). Byte-causality state of the detected harness; `""` whenever no harness is detected. |
| `child_pid` | Foreground pid when a harness is detected, else 0. |
| `model` | `PPZ_AGENT_MODEL` env var, `""` otherwise (detection is phase 4). |
| `hostname`, `os`, `arch`, `pid`, `ppz_version` | Wrapper host/runtime identity. |
| `started_at` | Wrapper start time, RFC 3339 UTC. |
| `interval_sec` | Nominal cadence (60). Liveness derives from beat age vs this: `< 1.5×` online, `< 3×` stale, else offline (`internal/daemon/heartbeat_status.go`). |

`ppz who --json` keeps `status` liveness-only; the table's combined
`online|working` form is presentation (`CombineHeartbeatStatus`), not wire.
See `docs/specs/agent-detection.md` for the detection design.
