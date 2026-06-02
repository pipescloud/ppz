#!/usr/bin/env bash
# `ack:read` envelopes emitted by a reader running inside `ppz
# terminal share` must stamp envelope.sender = the wrapped handle,
# NOT "". User-visible symptom from the field:
#
#   $ ppz read inbox
#   20:07:13  -             ack:read → ba39506c
#                ^^^                   ↑
#                empty sender          the ack itself
#   20:07:18  alan          Got it — test received loud and clear.
#                ^^^^
#                regular message DOES carry sender (post-PR-#92)
#
# Same root cause as the send bug PR #92 fixed: the daemon's read-
# path ack auto-emitter (emitAcks at read.go:316,371) resolves the
# reader's own handle via d.State.Current(req.Session) only, ignoring
# the env hint terminalShareEnv exports into the wrapped child. Inside
# the share, State.Current("alan") is empty (IPCCreate skips
# SetCurrent for PTY-kind sources) so the ack envelope ships with
# self="".
#
# Fix mirrors PR #92's send path: CLI forwards PPZ_CURRENT_HANDLE as
# ReadRequest.Sender; daemon's emit sites route the precedence through
# senderForRequest (hint wins, state fallback) — the same helper PR
# #92 introduced for the send side, reused here.
#
# End-to-end coverage closes the same positional-arg-swap blind spot
# that motivated the send-side e2e: unit tests can pin the CLI
# forwarding and the helper precedence separately, but only this
# fixture exercises the wire chain
#
#   cmdRead → ReadRequest.Sender → emit-site senderForRequest
#           → buildAckEnvelope → published envelope.sender on
#             <original-sender>.inbox
#
# against a live daemon.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

# jimmy: message-kind source. .inbox is the ack:read destination.
ppz_a source create jimmy >/dev/null
# alan: pty-kind source. terminal create writes State.Current(OUTER
# session)=alan; we'll then share BARE (no explicit handle) so share
# resolves the wrapped handle from current. (Sharing with explicit
# handle would try to re-create alan and fail E_SOURCE_TAKEN.)
ppz_a terminal create alan >/dev/null

# Jimmy sends to alan.inbox with --request-ack. PPZ_CURRENT_HANDLE=
# jimmy makes the send stamp sender=jimmy (so the ack has somewhere
# to go) AND satisfies the --request-ack preflight (which requires a
# resolvable current source via effectiveCurrentHandle).
PPZ_CURRENT_HANDLE=jimmy ppz_a send alan "ping" --request-ack >/dev/null

# Wait for the ping to land on alan.inbox.
wait_for 20 "ppz_a reread alan.inbox --json | grep -q ping" >/dev/null

# Alan reads inside a wrapped share. The bare share resolves
# current=alan and exports PPZ_SESSION=alan + PPZ_CURRENT_HANDLE=alan
# into the wrapped bash. The wrapped `ppz read alan.inbox` drains
# the message, advances cursor, and triggers the daemon's ack auto-
# emit goroutine — which (post-fix) stamps envelope.sender=alan from
# the request hint instead of looking up State.Current("alan" session)
# (which is empty).
ppz_a terminal share -- bash -c 'ppz read alan.inbox' >/dev/null 2>&1

# Wait for the ack:read to land on jimmy.inbox. emitAcks runs in a
# daemon goroutine after cursor advance, so the ack publish is
# decoupled from the share's lifecycle — by the time the share
# process exits the ack is in flight; wait_for absorbs the delivery
# latency.
wait_for 20 "ppz_a reread jimmy.inbox --json | grep -q 'ack:read'" >/dev/null

# Assert envelope.sender on the ack. Pre-fix: sender="". Post-fix:
# sender="alan".
ppz_a reread jimmy.inbox --json | jq -c 'select(.subject=="ack:read") | {sender, subject}'
