#!/usr/bin/env bash
# RED: the point of the upgrade — after bare `ppz terminal share` on a message
# source, someone must be able to lease/control it. This proves the upgraded
# source's `.system` pipe is not merely present but WIRED: the share process's
# lease manager consumes it and grants. `ppz terminal lease` exits 0 and the
# host publishes a lease-state naming the holder. Until bare-share provisions
# system, the acquire has nowhere to land and the CLI times out (non-zero).
. /tests/lib/common.sh

cur_holder() {
  ppz_a reread chat.system --json 2>/dev/null | jq -r '.payload' \
    | jq -r 'select(.type=="lease-state") | .holder' | tail -1
}

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

ppz_a terminal share -- sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^chat.stdout'" >/dev/null

PPZ_CURRENT_HANDLE=holder ppz_a terminal lease chat 60s >/dev/null 2>&1
echo "lease_rc: $?"
echo "lease_holder: $(cur_holder)"

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
