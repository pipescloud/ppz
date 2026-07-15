#!/usr/bin/env bash
# RED: a lease is time-bounded. After the TTL elapses the host expires it and
# publishes a free `lease-state` (empty holder), reopening .stdin to other
# senders WITHOUT an explicit release. Uses a 1s TTL. Not implemented yet.
. /tests/lib/common.sh

cur_holder() {
  ppz_a reread box.system --json 2>/dev/null | jq -r '.payload' \
    | jq -r 'select(.type=="lease-state") | .holder' | tail -1
}

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.stdin'" >/dev/null

PPZ_CURRENT_HANDLE=holder ppz_a terminal lease box 1s >/dev/null 2>&1

# Blocked during the (short) lease.
PPZ_CURRENT_HANDLE=intruder ppz_a send box.stdin $'during-lease\n' >/dev/null

# Wait for the host to expire the lease (free state).
wait_for 50 'test -z "$(cur_holder)"' >/dev/null

PPZ_CURRENT_HANDLE=intruder ppz_a send box.stdin $'after-expiry\n' >/dev/null
wait_for 30 "ppz_a reread box.stdout 2>/dev/null | grep -q after-expiry" >/dev/null

echo "during_lease_delivered: $(ppz_a reread box.stdout | grep -c during-lease)"
echo "after_expiry_delivered: $(ppz_a reread box.stdout | grep -c after-expiry)"

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
