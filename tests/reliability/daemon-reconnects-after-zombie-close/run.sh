#!/usr/bin/env bash
# RED — self-healing half of the 2026-06-11 wake-from-sleep incident
# (ppz-diag-20260611-073803.tgz).
#
# At 07:37:03 in that incident, a JetStream call timed out on the
# daemon's zombie post-sleep connection and reportNATSFailure() closed
# the NC — deliberately, with NO replacement: recovery is deferred to
# "the next ensureNATS call". But ensureNATS only runs when a CLI
# command happens to arrive. A daemon whose connection was failure-
# closed sits disconnected indefinitely — `ppz status` honestly says
# "disconnected" while doing nothing about it — until the user types a
# recovery-triggering command or the next JWT rotation fires.
#
# Contract pinned here: after a failure-close, once the server is
# reachable again the daemon must re-establish its NATS connection BY
# ITSELF, promptly, with no CLI traffic other than read-only status
# polls (ipcStatus never touches ensureNATS/rebuildNC — verified
# against internal/daemon/ipc.go).
#
# Mechanics: docker pause (not stop — stop sends FIN and the nats.go
# client auto-reconnects; pause leaves a silent zombie, the sleep
# signature) + a send whose JetStream existence check times out and
# triggers reportNATSFailure. Then unpause and watch status.
#
# Timing guard: the reliability overlay sets PPZ_NATS_JWT_TTL=120s, so
# the refresh loop's rotation at login+90s would rescue the connection
# anyway (OnRefreshed → rebuildNC). The status poll below must finish
# before that — we poll ~30s, ending well under login+90s — so a PASS
# can only come from genuine self-healing, not the rotation timer.
. /tests/lib/common.sh

HANDLE="zombroom"

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a unset handle    >/dev/null 2>&1
ppz_a unset namespace >/dev/null 2>&1
ppz_a pipe create "$HANDLE" >/dev/null

# Sanity: the happy path works while the server is up, so the failure
# below is attributable to the pause, not broken setup.
up=$(mktemp)
PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz send "$HANDLE" "while-up" >/dev/null 2>"$up"
if grep -qE '^sent\b' "$up"; then echo "setup=OK"; else echo "setup=BROKEN"; fi
rm -f "$up"

# Freeze the server: TCP stays up (no FIN), JetStream goes silent — the
# zombie-connection state a sleeping laptop leaves behind.
docker pause compose-ppz-server-1 >/dev/null
sleep 2

# This send's JetStream existence check times out (~5s) and trips
# reportNATSFailure, which closes the NC without a replacement. The
# send must fail; its exact error code is send-fails-fast's contract,
# not ours.
if PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" timeout 25 ppz send "$HANDLE" "while-frozen" >/dev/null 2>&1; then
  echo "send-during-pause=UNEXPECTED-OK"
else
  echo "send-during-pause=FAILED"
fi

# Premise guard: the failure-close must have actually happened — the
# daemon should report disconnected (a closed NC renders as
# "disconnected"). Checked while the server is STILL PAUSED: recovery
# cannot complete against a frozen server, so the read is
# deterministic. (Checking after unpause races a fast self-heal — the
# very behaviour this scenario demands.) If still "connected" the send
# never tripped reportNATSFailure and the verdict below is meaningless.
# `matches`, not `grep -q`: a short-circuiting grep can SIGPIPE ppz
# (Go writes stdout a line per syscall), and that 141 would silently
# take the else branch — i.e. fake the result this guard exists to
# verify. See matches in common.sh.
if ppz_a status | matches '^nats: connected'; then
  echo "zombie-closed=NO"
else
  echo "zombie-closed=YES"
fi

docker unpause compose-ppz-server-1 >/dev/null

# The server is reachable again. The daemon must reconnect on its own.
# Poll read-only status for ~30s (300 attempts x 0.1s + command time)
# — comfortably past any sane self-heal backoff, comfortably short of
# the login+90s rotation rescue.
if wait_for 300 'ppz_a status | grep -q "^nats: connected"'; then
  echo "verdict=RECONNECTED"
else
  echo "verdict=STUCK-DISCONNECTED"
fi
