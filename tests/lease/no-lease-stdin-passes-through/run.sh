#!/usr/bin/env bash
# GUARD (green now, must stay green): with NO lease held, .stdin enforcement
# is inert -- any sender's bytes reach the child exactly as today, so existing
# `ppz send`/`command` keep working. Pins that lease-checking only activates
# while a lease is held. Passes today for the "right eventual reason"; locks
# the contract so the GREEN implementation can't over-enforce.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.stdin'" >/dev/null

PPZ_CURRENT_HANDLE=anyone ppz_a send box.stdin $'no-lease-line\n' >/dev/null
wait_for 30 "ppz_a reread box.stdout 2>/dev/null | grep -q no-lease-line" >/dev/null

echo "delivered_without_lease: $(ppz_a reread box.stdout | grep -c no-lease-line)"

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
