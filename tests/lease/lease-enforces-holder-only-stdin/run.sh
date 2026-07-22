#!/usr/bin/env bash
# RED: while a lease is held by `holder`, the PTY host forwards .stdin bytes
# ONLY from the lease-holder; a different sender's bytes are dropped before
# reaching the child. FIFO ordering on .stdin makes the negative assertion
# deterministic: we publish the interloper's line FIRST, then the holder's;
# once the holder's line surfaces on .stdout, the interloper's has already
# been processed-and-dropped. Not implemented yet (no enforcement -> both
# lines currently reach the child, so intruder_delivered is 1).
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.stdin'" >/dev/null

PPZ_CURRENT_HANDLE=holder ppz_a terminal lease box 60s >/dev/null 2>&1

PPZ_CURRENT_HANDLE=intruder ppz_a send box.stdin $'from-intruder\n' >/dev/null
PPZ_CURRENT_HANDLE=holder   ppz_a send box.stdin $'from-holder\n'   >/dev/null
wait_for 30 "ppz_a reread box.stdout 2>/dev/null | grep -q from-holder" >/dev/null

echo "intruder_delivered: $(ppz_a reread box.stdout | grep -c from-intruder)"
echo "holder_delivered: $(ppz_a reread box.stdout | grep -c from-holder)"

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
