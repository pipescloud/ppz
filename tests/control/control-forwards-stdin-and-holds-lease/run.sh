#!/usr/bin/env bash
# RED: `ppz terminal control <handle>` is interactive attach -- it follows
# .stdout (like watch) AND forwards the operator's keystrokes to .stdin,
# after acquiring the write-lease. We feed it a line on stdin and assert
# (a) the child received it and (b) the host granted a lease to control's
# sender. Not implemented yet.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.stdin'" >/dev/null

# Drive control with a piped keystroke stream; keep stdin open ~3s so the
# session stays attached while the byte round-trips, then let it close.
( printf 'typed-in-control\n'; sleep 3 ) \
  | PPZ_CURRENT_HANDLE=ctrlr PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" \
    ppz terminal control box >/tmp/ctl.out 2>&1 &
CTL_PID=$!

wait_for 30 "ppz_a reread box.stdout 2>/dev/null | grep -q typed-in-control" >/dev/null

echo "keystroke_delivered: $(ppz_a reread box.stdout | grep -c typed-in-control)"
lease_holder=$(ppz_a reread box.system --json 2>/dev/null | jq -r '.payload' \
  | jq -r 'select(.type=="lease-state") | .holder' | tail -1)
echo "lease_holder: $lease_holder"

kill "$CTL_PID" 2>/dev/null || true
kill "$TERM_PID" 2>/dev/null || true
wait "$CTL_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
