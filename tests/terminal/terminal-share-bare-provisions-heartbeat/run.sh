#!/usr/bin/env bash
# RED: bare `ppz terminal share` (no handle) must provision <handle>.heartbeat
# on the current source AND publish beats to it — otherwise the wrapped
# terminal is invisible to `ppz who`. The bare path only ensures
# stdin/stdout/stdctrl today, so on a source that wasn't born pty-kind (here a
# `source create`d message source) the heartbeat pipe is never created and the
# wrapper's beats are silently dropped.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

# Bare share (no handle → current source 'chat'), long-lived child so the
# wrapper's first heartbeat has time to fire.
ppz_a terminal share -- sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^chat.stdout'" >/dev/null

# Subshell with pipefail off: `heartbeat` sorts FIRST among the pty
# pipes, so grep -q matches row 1 and exits while ls_normalize's
# awk/sed still have five rows to write — they take SIGPIPE, and under
# common.sh's `set -o pipefail` that 141 becomes the pipeline status
# and reports "missing" for a pipe that is right there. Same trap
# wait_for documents (measured ~3% of runs on the alpine test image).
if (set +o pipefail; ppz_a ls | ls_normalize | grep -q '^chat.heartbeat'); then
  echo "heartbeat_pipe: present"
else
  echo "heartbeat_pipe: missing"
fi

if wait_for 30 "ppz_a reread chat.heartbeat --json 2>/dev/null | jq -e '.payload' >/dev/null 2>&1"; then
  echo "heartbeat_published: yes"
else
  echo "heartbeat_published: no"
fi

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
