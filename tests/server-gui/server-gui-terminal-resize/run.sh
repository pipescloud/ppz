#!/usr/bin/env bash
# "Fit to window" on the web terminal sends a setsize the server publishes
# on <handle>.stdctrl, just like `ppz terminal resize`. It needs write on
# stdctrl (not the control lease): with enforcement on, a viewer granted
# only stdin write can take control but not resize.
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

echo "--- member, enforcement off ---"
auth_as_foo
term_ws alpha box expect:control setsize:150x45 sleep:1s
wait_for 50 '[ "$(host_size)" = "150x45" ]' >/dev/null
echo "host reports: $(host_size)"

echo "--- enforcement on, bar has read + stdin write only ---"
ppz_a acl enforce on >/dev/null
ppz_a pipe acl grant box.stdout bar read >/dev/null
ppz_a pipe acl grant box.stdin bar write >/dev/null
auth_as_bar
term_ws alpha box expect:control setsize:90x20 expect:control-error
echo "host reports: $(host_size)"

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
