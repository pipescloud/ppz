#!/usr/bin/env bash
# Web terminal access is the ACL's. With enforcement on, foo's shared
# terminal is owner-only: bar can't open it at all; with read on stdout
# bar can watch but not take control (the server refuses even a hand-
# crafted take); with write on stdin bar can take control.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.stdin'" >/dev/null
ppz_a acl enforce on >/dev/null

auth_as_bar
echo "--- no grants ---"
term_ws alpha box expect:control || true

echo "--- read on stdout ---"
ppz_a pipe acl grant box.stdout bar read >/dev/null
term_ws alpha box expect:control take expect:control-error

echo "--- write on stdin ---"
ppz_a pipe acl grant box.stdin bar write >/dev/null
term_ws alpha box expect:control take expect:lease release

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
