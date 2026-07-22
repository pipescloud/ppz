#!/usr/bin/env bash
# RED: a second acquirer is denied while the lease is held. `ppz terminal
# lease` by a different sender exits non-zero (E_LEASE_HELD = 27) and reports
# the current holder, rather than stealing the lease. Not implemented yet.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.stdin'" >/dev/null

PPZ_CURRENT_HANDLE=owner ppz_a terminal lease box 60s >/dev/null 2>&1

PPZ_CURRENT_HANDLE=other ppz_a terminal lease box 60s >/tmp/deny.out 2>&1
echo "second_rc: $?"
grep -oE 'held by [a-z-]+' /tmp/deny.out | head -1

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
