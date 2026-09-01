# ppz Error Codes & Exit Codes

This table is **authoritative**. CLI exit codes, stderr messages, JSON-RPC
error codes, and HTTP error response codes all derive from it.

## Format

- Stderr (one line, then newline): `error: <CODE>: <message>`
- HTTP body: `{"error": {"code": "<CODE>", "message": "<message>"}}`
- JSON-RPC error: `{"code": <exit_code_int>, "message": "<CODE>: <message>"}`
- Stdout is **never** written when an error occurs.

## Codes

| Exit | Code | When | HTTP status |
|---:|---|---|---:|
| 0  | — | success | 2xx |
| 1  | (none) | unexpected internal error | 500 |
| 2  | (none) | usage / argument parse error | — |
| 10 | `E_NOT_LOGGED_IN` | daemon has no stored credential | — |
| 11 | `E_DAEMON_NOT_RUNNING` | CLI cannot reach the IPC socket | — |
| 12 | `E_INVALID_API_KEY` | server rejected the api key | 401 |
| 13 | `E_SOURCE_TAKEN` | source with this handle already exists in this org | 409 |
| 14 | `E_SOURCE_NOT_FOUND` | no source with this handle in this org | 404 |
| 15 | `E_INVALID_HANDLE` | source handle fails regex or is reserved | 400 |
| 16 | `E_NO_CURRENT_SOURCE` | broadcast attempted with no `current` source set | — |
| 17 | `E_PAYLOAD_TOO_LARGE` | encoded envelope > 65 536 bytes | 413 |
| 18 | `E_SERVER_UNREACHABLE` | daemon could not contact `ppz-server` (HTTP) | 502 |
| 19 | `E_NATS_UNREACHABLE` | daemon could not publish to NATS | — |
| 20 | `E_INVALID_PIPE` | `read`/`send`/`broadcast` target had a malformed or unsupported pipe name | 400 |
| 21 | `E_PIPE_TAKEN` | pipe with this name already exists on this source | 409 |
| 22 | `E_PIPE_NOT_FOUND` | no pipe with this name on this source | 404 |
| 23 | `E_INVALID_SUBJECT` | `--subject` value violates a reserved-prefix rule (the `ack:` prefix is daemon-internal) | 400 |
| 24 | `E_INVALID_MANIFOLD` | a manifold (namespace) path segment fails the handle regex | 400 |
| 25 | `E_DELIVERY_UNCONFIRMED` | `send` published but the server did not ack the PubAck in time (may or may not have landed) | — |
| 26 | `E_DAEMON_TIMEOUT` | the local daemon accepted the IPC connection but did not reply within the deadline | — |
| 27 | `E_LEASE_HELD` | `terminal lease`: a different controller already holds the terminal's write-lease | — |
| 28 | `E_LEASE_NO_HOST` | `terminal lease`/`control`: acquire published but no terminal host answered (offline, or a ppz predating terminal control) | — |
| 30 | `E_CREDENTIAL_TOO_LARGE` | the minted NATS User JWT exceeds the server's `max_control_line`, so NATS refuses the connection before authenticating | — |

Exit codes 21 and 22 are reserved here for the user-creatable-pipes feature
that lands in a later phase; nothing in the current code path returns them
yet.

## Standard messages (for stderr)

Codes that reference a specific source/pipe interpolate the offending name
so users see *which* one, not just "source not found". Generic codes (no
entity name available) keep the static message.

| Code | Message format |
|---|---|
| `E_NOT_LOGGED_IN` | `not logged in; run 'ppz daemon login URL -apikey K'` |
| `E_DAEMON_NOT_RUNNING` | `daemon not running; run 'ppz daemon start'` |
| `E_INVALID_API_KEY` | `invalid api key` |
| `E_SOURCE_TAKEN` | `source '<handle>' already exists in this org` |
| `E_SOURCE_NOT_FOUND` | `source '<handle>' not found` |
| `E_INVALID_HANDLE` | `invalid handle '<handle>': must match [a-z0-9-] (max 32, no leading/trailing -, not reserved)` |
| `E_NO_CURRENT_SOURCE` | `no current source for this shell session; run 'ppz source create <handle>' (or 'ppz source switch <handle>' to point at an existing one); if you're driving ppz from agent subprocesses with no shared tty, export PPZ_SESSION=<id> consistently across calls so they share session state` |
| `E_PAYLOAD_TOO_LARGE` | `payload too large; max 64KiB encoded` |
| `E_SERVER_UNREACHABLE` | `server unreachable` |
| `E_NATS_UNREACHABLE` | `nats unreachable; common causes: expired credentials (try 'ppz daemon logout' then re-login), or on non-docker setups missing PPZ_NATS_URL=nats://localhost:4222` |
| `E_CREDENTIAL_TOO_LARGE` | `nats credential too large; this principal's compiled ACL grants exceed the server's max control line, so NATS refuses the connection before authenticating. Re-login cannot help — the new credential is the same size. Raise max_control_line on ppz-server, or narrow this principal's pipe grants` |
| `E_INVALID_PIPE` (reserved) | `pipe name '<name>' is reserved` |
| `E_INVALID_PIPE` (regex) | `pipe name '<name>' is invalid: must match [a-z0-9-] (max 32, no leading/trailing -)` |
| `E_INVALID_PIPE` (other) | `invalid pipe; check for typos, or for custom pipes run 'ppz pipe create <handle>.<name>' first` |
| `E_PIPE_TAKEN` | `pipe '<name>' already exists on source '<handle>'` |
| `E_PIPE_NOT_FOUND` | `pipe '<name>' not found on source '<handle>'` |
| `E_INVALID_SUBJECT` | `invalid subject; the 'ack:' prefix is reserved for system-emitted protocol messages` |
