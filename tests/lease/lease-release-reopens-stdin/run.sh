#!/usr/bin/env bash
# RED: `ppz terminal release` (by the holder) clears the lease; the host
# publishes a `lease-state` with an empty holder, after which any sender's
# .stdin flows again. release lands on .system while stdin lands on .stdin
# (different pipes -> no cross-FIFO), so we synchronize the post-release send
# on observing the host's free (empty-holder) event. Not implemented yet.
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

PPZ_CURRENT_HANDLE=holder ppz_a terminal lease box 60s >/dev/null 2>&1

# Interloper blocked while the lease is held.
PPZ_CURRENT_HANDLE=intruder ppz_a send box.stdin $'before-release\n' >/dev/null

# Holder releases; wait until the host reflects the free (empty-holder) state.
PPZ_CURRENT_HANDLE=holder ppz_a terminal release box >/dev/null 2>&1
wait_for 30 'test -z "$(cur_holder)"' >/dev/null

# Now the same interloper's bytes flow.
PPZ_CURRENT_HANDLE=intruder ppz_a send box.stdin $'after-release\n' >/dev/null
wait_for 30 "ppz_a reread box.stdout 2>/dev/null | grep -q after-release" >/dev/null

echo "before_release_delivered: $(ppz_a reread box.stdout | grep -c before-release)"
echo "after_release_delivered: $(ppz_a reread box.stdout | grep -c after-release)"

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
