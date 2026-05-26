#!/usr/bin/env bash
# Production scenario reported 2026-05-26 (copilot/alex pty session):
# while ppz-server was being restarted, `ppz send` HUNG — the operator
# observed >2 minutes of no output, no error, and no non-zero exit. Yet
# `ppz read` kept working throughout. The agent invented a "transient
# glitch self-resolved" story; the human correctly objected that
# `ppz send` should error if it can't reach the server.
#
# Root cause (two missing timeouts, one default):
#   1. internal/daemon/ipc.go Call() sets no read deadline on the unix
#      socket. net.Dial succeeds the moment the daemon accepts; if the
#      handler then stalls, dec.Decode(&resp) blocks forever.
#   2. internal/daemon/handlers.go handleSend publishes then calls
#      NC.Flush() with no timeout and only replies AFTER it returns.
#      During a server outage the NATS conn is reconnecting, so Flush
#      blocks on a PONG that never comes -> no IPC reply -> client (1)
#      hangs. (NC.Publish itself buffers into the 8MB reconnect buffer
#      and returns nil, which also explains the earlier "exit 0 but the
#      message never landed" reports.)
#   `ppz read` is served from local daemon state with no NATS round-trip,
#   so it is the only common verb that does NOT hang.
#
# Fix: bound the client IPC wait (read deadline / context) AND give the
# daemon a flush timeout (NC.FlushTimeout / FlushWithContext) so send
# fails fast with E_NATS_UNREACHABLE instead of hanging.
#
# Test: create a target while the server is up, stop ppz-server (which
# embeds NATS), then send to it under a short `timeout` watchdog. Assert
# the command RETURNED (was not killed by the watchdog) and surfaced an
# E_NATS_UNREACHABLE error. Mirrors the
# uncollared-pipe-survives-ppz-server-restart docker-stop pattern.
. /tests/lib/common.sh

HANDLE="fastfailroom"

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a unset handle    >/dev/null 2>&1
ppz_a unset namespace >/dev/null 2>&1

# Create the destination while the server is still up so target
# resolution is cached locally — this isolates the bug to the publish
# path, not target lookup.
ppz_a pipe create "$HANDLE" >/dev/null
ppz_a send "$HANDLE" "before-down" >/dev/null 2>&1

# Take the server (embedded NATS + JetStream) down and leave it down.
# The reliability suite's run.sh `docker start`s it again before the
# next scenario, so we don't need to restart it here.
docker stop compose-ppz-server-1 >/dev/null
# Give the daemon a moment to observe the disconnect so the send hits
# the reconnecting (buffering / blocked-flush) state, not a still-healthy
# connection.
sleep 2

# The user's exact symptom: send while the server is unreachable. Wrap
# it in a watchdog well under the harness ceiling. If send hangs, the
# watchdog kills it with exit 124 and we record verdict=HANG (the bug).
# If send returns promptly, we record verdict=RETURNED plus the error
# token, regardless of the exact byte/id values.
err=$(mktemp)
timeout 25 ppz_a send "$HANDLE" "while-down" 2>"$err"
rc=$?
if [ "$rc" -eq 124 ]; then
  echo "verdict=HANG"
else
  echo "verdict=RETURNED"
fi
grep -oE '^error: E_[A-Z_]+' "$err" | head -1
rm -f "$err"
