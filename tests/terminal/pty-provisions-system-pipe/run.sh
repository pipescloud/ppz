#!/usr/bin/env bash
# RED: a pty source must auto-provision a `.system` control-plane pipe
# alongside inbox/stdin/stdout/stdctrl/heartbeat. `.system` carries lease
# acquire/release requests (writer -> host) and host-published lease-state
# events (host -> observers). Not implemented yet, so `.system` is absent.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.stdout'" >/dev/null

if ppz_a ls | ls_normalize | matches '^box.system'; then
  echo "system_pipe: present"
else
  echo "system_pipe: missing"
fi

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
