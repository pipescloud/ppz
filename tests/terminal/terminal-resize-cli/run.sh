#!/usr/bin/env bash
# `ppz terminal resize [H] COLSxROWS` publishes a setsize on H.stdctrl;
# the sharing host applies it to the child pty and republishes the new
# size as a "resize" every viewer follows. H defaults to the current
# source.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.stdin'" >/dev/null

host_size() {
  ppz_a reread box.stdctrl --json 2>/dev/null | jq -r '.payload' \
    | jq -r 'select(.type=="resize") | "\(.cols)x\(.rows)"' | tail -1
}

echo "--- explicit handle ---"
ppz_a terminal resize box 120x40
wait_for 50 '[ "$(host_size)" = "120x40" ]' >/dev/null
echo "host reports: $(host_size)"

echo "--- current source ---"
ppz_a set handle box >/dev/null
ppz_a terminal resize 100x30
wait_for 50 '[ "$(host_size)" = "100x30" ]' >/dev/null
echo "host reports: $(host_size)"

echo "--- invalid size ---"
ppz_a terminal resize box 120 >/dev/null 2>&1; echo "exit: $?"

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
