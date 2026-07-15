#!/usr/bin/env bash
# RED: `ppz terminal lease <handle> <dur>` acquires an advisory write-lease.
# It stamps its sender from PPZ_CURRENT_HANDLE, publishes a lease-acquire on
# <handle>.system, blocks until the PTY host grants, and the host publishes a
# `lease-state` naming the holder. We assert the grant is observable on
# <handle>.system. Not implemented yet.
. /tests/lib/common.sh

cur_holder() {
  ppz_a reread box.system --json 2>/dev/null | jq -r '.payload' \
    | jq -r 'select(.type=="lease-state") | .holder' | tail -1
}

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.stdout'" >/dev/null

PPZ_CURRENT_HANDLE=holder ppz_a terminal lease box 60s >/dev/null 2>&1

echo "lease_holder: $(cur_holder)"

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
