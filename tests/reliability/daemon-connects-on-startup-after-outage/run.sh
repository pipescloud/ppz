#!/usr/bin/env bash
# Regression GUARD (currently GREEN, shipped in #128 / v0.48.2) — the
# retry-through-outage half of cold-start connect.
#
# daemon-connects-on-startup proves a freshly-restarted daemon self-
# connects when ppz-server is UP, but that path never exercises the
# retry loop: the very first cold /auth/exchange succeeds. The real
# `ppz upgrade` / reboot case is messier — the daemon process restarts
# while the server (or the network) is briefly unreachable. Pre-#128
# two things broke this:
#   1. Run() had no proactive connect at all (the startup gap), and
#   2. even once connectOnStartup existed, the cold /auth/exchange
#      mapped EVERY non-200 to a terminal EInvalidAPIKey, so a single
#      transient 5xx during the outage made kickConnect give up
#      FOREVER (fixed in e085332: retry transient, terminal only on
#      401/403).
#
# Contract pinned here: a daemon that restarts while the server is
# unreachable keeps retrying the cold bootstrap with backoff and brings
# NATS up BY ITSELF once the server returns — with no connection-
# triggering CLI command in between.
#
# Why this is the cleanest possible self-heal assertion: a cold-
# restarted daemon has NO refresh loop running yet (the loop is only
# started inside bootstrapNATS after a successful cold exchange — see
# internal/daemon/handlers.go). So unlike daemon-reconnects-after-
# zombie-close, there is no JWT-rotation timer that could rescue the
# connection behind the test's back. The ONLY thing that can connect a
# cold daemon through an outage is the kickConnect("startup") backoff
# loop — exactly what we're guarding.
#
# Probe discipline: we poll `ppz status` ONLY. status does an HTTP
# login-check, not a NATS call (internal/daemon/ipc.go ipcStatus), so
# it never routes through ensureNATS and cannot itself establish the
# connection and mask the gap. `ls`/`send` would.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

# Prove the happy path while up — also a setup guard. If `ls` doesn't
# work here the post-restart assertions are meaningless.
if ppz_a ls >/dev/null 2>&1; then echo "setup=OK"; else echo "setup=BROKEN"; fi

# Take the server down BEFORE the daemon restarts, so the new process's
# first cold /auth/exchange attempts hit an unreachable server. The
# reliability runner `docker start`s ppz-server again before the next
# scenario, but we bring it back ourselves below to observe the self-heal.
docker stop compose-ppz-server-1 >/dev/null

# Restart the daemon PROCESS (what `ppz upgrade` / a reboot does). It
# reads persisted credentials from the home volume and must begin
# retrying the cold connect against the (currently down) server.
docker restart compose-daemon-a-1 >/dev/null

# Wait for the new daemon to accept IPC again. `ppz status` succeeding
# (rc 0) only needs the socket — not a NATS connection — so this loop
# does not itself trigger a connect.
wait_for 300 'ppz_a status >/dev/null 2>&1'

# Premise guard: with the server down the daemon cannot connect, so it
# must honestly report not-connected. Checked while the server is STILL
# DOWN so the read is deterministic (checking after start would race the
# self-heal we want to observe). If this says connected, the outage
# wasn't real and the verdict below would be meaningless.
# `matches`, not `grep -q`: a short-circuiting grep can SIGPIPE ppz
# (Go writes stdout a line per syscall), and that 141 would silently
# take the else branch — i.e. fake the result this guard exists to
# verify. See matches in common.sh.
if ppz_a status | matches '^nats: connected'; then
  echo "down-state=CONNECTED-UNEXPECTED"
else
  echo "down-state=NOT-CONNECTED"
fi

# Bring the server back. The retrying startup loop must now succeed on
# its next backoff iteration — no CLI command other than status polls.
docker start compose-ppz-server-1 >/dev/null
# Wait for the server's HTTP healthcheck before we start judging, so the
# poll window covers the daemon's reconnect, not the server's boot.
for i in $(seq 1 30); do
  if curl -sf http://ppz-server:8080/healthz >/dev/null 2>&1; then break; fi
  sleep 1
done

# Poll read-only status (NEVER ls/send) for the self-established
# connection. reconnectMaxBackoff is 15s, so 300 attempts x 0.1s plus
# per-command time comfortably covers a couple of backoff cycles.
if wait_for 300 'ppz_a status | grep -q "^nats: connected"'; then
  echo "verdict=CONNECTED-AFTER-OUTAGE"
else
  echo "verdict=STUCK-DISCONNECTED"
fi
