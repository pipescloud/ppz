#!/usr/bin/env bash
# One controller at a time: while foo holds the lease from the browser,
# bar sees who has it, a take is refused, and bar's keystrokes are
# dropped server-side rather than reaching the terminal. Closing foo's
# socket releases the lease.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.stdin'" >/dev/null

holder() {
  ppz_a reread box.system --json 2>/dev/null | jq -r '.payload' \
    | jq -r 'select(.type=="lease-state") | .holder // ""' | tail -1
}

auth_as_foo
term_ws alpha box take expect:lease sleep:6s >/dev/null &
FOO_PID=$!
wait_for 50 '[ "$(holder)" = "foo@web" ]' >/dev/null

auth_as_bar
term_ws alpha box expect:lease take expect:control-error send:bar-sneaks-in sleep:1s

echo "bar input delivered: $(ppz_a reread box.stdout 2>/dev/null | grep -c bar-sneaks-in)"
wait "$FOO_PID" 2>/dev/null || true
wait_for 50 '[ -z "$(holder)" ]' >/dev/null
h="$(holder)"
echo "lease after foo disconnects: ${h:-free}"

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
