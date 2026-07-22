#!/usr/bin/env bash
# RED: a lease is time-bounded. After the TTL elapses the host expires it and
# publishes a free `lease-state` (empty holder), reopening .stdin to other
# senders WITHOUT an explicit release. Uses a 3s TTL — long enough that the
# during-lease drop is observed well inside the window (see the holder-sync
# barrier below), short enough to keep the test quick. Not implemented yet.
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

PPZ_CURRENT_HANDLE=holder ppz_a terminal lease box 3s >/dev/null 2>&1

# Intruder byte on .stdin while the lease is held, immediately followed by a
# holder byte on the SAME FIFO. Single in-order consumer: when holder-sync
# surfaces on .stdout the intruder byte has already been processed — and, since
# it landed inside the lease window, dropped. This makes during_lease_delivered
# deterministic instead of racing the free-state wait against .stdin drain.
PPZ_CURRENT_HANDLE=intruder ppz_a send box.stdin $'during-lease\n' >/dev/null
PPZ_CURRENT_HANDLE=holder   ppz_a send box.stdin $'holder-sync\n'  >/dev/null
wait_for 30 "ppz_a reread box.stdout 2>/dev/null | grep -q holder-sync" >/dev/null

# Wait for the host to expire the lease (free state) — no explicit release.
wait_for 50 'test -z "$(cur_holder)"' >/dev/null

PPZ_CURRENT_HANDLE=intruder ppz_a send box.stdin $'after-expiry\n' >/dev/null
wait_for 30 "ppz_a reread box.stdout 2>/dev/null | grep -q after-expiry" >/dev/null

echo "during_lease_delivered: $(ppz_a reread box.stdout | grep -c during-lease)"
echo "after_expiry_delivered: $(ppz_a reread box.stdout | grep -c after-expiry)"

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
