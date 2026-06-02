#!/usr/bin/env bash
# Regression GUARD (currently GREEN) — the steady-state half of the
# 2026-05-26 copilot/alex report, where `ppz send` HUNG while ppz-server
# was being restarted. Investigation showed a cleanly-stopped OR frozen
# (docker pause) server does NOT hang: the send path's pre-publish
# JetStream existence check and the daemon's 5s HTTP client both time
# out, so send fast-fails with E_NATS_UNREACHABLE within ~5s. This test
# locks in that contract: when ppz-server is unreachable, `ppz send`
# must fail fast and HONESTLY — never hang, never silently "succeed".
#
# It does NOT reproduce the production hang itself. That hang was the
# narrower daemon-restart/startup window, and its true root cause — the
# IPC client (internal/daemon/ipc.go Call) having no read deadline, so a
# stalled daemon hangs the CLI regardless of why — is covered
# deterministically by the unit test
# internal/daemon/ipc_call_timeout_test.go. Keep both: the unit test is
# the RED reproduction of the defect; this is the end-to-end guard that
# the normal server-down path stays fast and explicit.
#
# Mirrors the uncollared-pipe-survives-ppz-server-restart docker-stop
# pattern.
. /tests/lib/common.sh

HANDLE="fastfailroom"

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a unset handle    >/dev/null 2>&1
ppz_a unset namespace >/dev/null 2>&1

# Create the destination while the server is still up so target
# resolution is cached locally — this isolates the bug to the publish
# path, not target lookup.
ppz_a pipe create "$HANDLE" >/dev/null

# Prove the happy path WORKS while the server is up — this doubles as a
# guard that login + seeding actually succeeded. If this isn't `sent`,
# the down-send assertion below is meaningless (a not-logged-in daemon
# would also "fail" the send), so surface setup=BROKEN and bail the
# diff loudly rather than drawing a false conclusion.
up=$(mktemp)
PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz send "$HANDLE" "while-up" >/dev/null 2>"$up"
if grep -qE '^sent\b' "$up"; then echo "setup=OK"; else echo "setup=BROKEN"; fi
rm -f "$up"

# Take the server (embedded NATS + JetStream) down and leave it down.
# The reliability suite's run.sh `docker start`s it again before the
# next scenario, so we don't need to restart it here.
docker stop compose-ppz-server-1 >/dev/null
# Give the daemon a moment to observe the disconnect so the send hits
# the reconnecting (buffering / blocked-flush) state, not a still-healthy
# connection.
sleep 2

# The user's exact symptom: send while the server is unreachable. Wrap
# it in a watchdog well under the harness ceiling and classify the
# outcome. The two known bug manifestations are BOTH wrong:
#   verdict=HANG       — watchdog had to kill it (the >2min stall report)
#   verdict=SILENT-OK  — returned exit 0 with a `sent` line while the
#                        server was down: NC.Publish buffered into the
#                        reconnect buffer and lied about delivery (the
#                        "exit 0 but the message never landed" report)
# The only correct outcome is a prompt, explicit failure:
#   verdict=ERROR      — returned non-zero with E_NATS_UNREACHABLE
# The success line goes to STDERR, so capture stderr and route stdout
# to /dev/null; classify off the verdict token, not byte/id values.
# NB: invoke `ppz` directly (not the ppz_a shell function) — `timeout`
# execs its argument as a program and cannot call a bash function, which
# silently exits 127 and never runs the send. Inline the socket env the
# same way ppz_a does.
err=$(mktemp)
PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" timeout 25 ppz send "$HANDLE" "while-down" >/dev/null 2>"$err"
rc=$?
if [ "$rc" -eq 124 ]; then
  echo "verdict=HANG"
elif grep -qE '^sent\b' "$err"; then
  echo "verdict=SILENT-OK"
elif grep -qE '^error: E_(NATS_UNREACHABLE|SERVER_UNREACHABLE|DELIVERY_UNCONFIRMED)\b' "$err"; then
  # Normalised "honest delivery failure" — either E_NATS_UNREACHABLE
  # (the existence check or connection layer fast-failed; the message
  # never left) or E_DELIVERY_UNCONFIRMED (publishWithAck got no
  # PubAck) is contract-compliant. Pinning the exact code would flake
  # on internal ordering shifts (e.g. resolveSendTarget's stream check
  # vs. publishWithAck's ack timeout) without any contract change.
  echo "verdict=ERROR-DELIVERY"
elif grep -qE '^error: E_' "$err"; then
  echo "verdict=ERROR-OTHER"
else
  echo "verdict=OTHER"
fi
rm -f "$err"
