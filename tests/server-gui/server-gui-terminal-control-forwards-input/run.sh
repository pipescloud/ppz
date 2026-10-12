#!/usr/bin/env bash
# Web terminal control: a member who may write the terminal's stdin takes
# control from the browser, the server acquires the write-lease on their
# behalf (holder "<username>@web"), and keystrokes reach the shared
# process as stdin envelopes from that sender. Release frees the lease.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.stdin'" >/dev/null

auth_as_foo
term_ws alpha box expect:control take expect:lease send:typed-in-browser sleep:1s release expect:lease

wait_for 30 "ppz_a reread box.stdout 2>/dev/null | grep -q typed-in-browser" >/dev/null
echo "delivered: $(ppz_a reread box.stdout | grep -c typed-in-browser)"
echo "stdin sender: $(ppz_a reread box.stdin --json | jq -r .sender | sort -u)"

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
