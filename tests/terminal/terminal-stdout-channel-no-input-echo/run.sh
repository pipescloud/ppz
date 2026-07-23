#!/usr/bin/env bash
# The PTY allocated by `ppz terminal share` must NOT echo input bytes back
# onto the master read side — otherwise responses that the local terminal
# emits to escape queries from the child (DA1, focus events, cursor-position
# reports, etc.) get republished to .stdout and pollute every viewer.
#
# Verifies: pipe a known input string in, run a child that consumes stdin
# silently and writes a different known output. The .stdout stream should
# contain ONLY the child's output, never the input.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

# Child: head -n 1 reads stdin to /dev/null, then printf writes "OUTPUT".
# `head -n 1` exits cleanly after one line, so we don't need to propagate
# stdin EOF through the PTY (which we currently can't).
# `timeout` is belt-and-braces: if echo is still on AND something else
# hangs, fail loudly instead of stalling the suite.
timeout 15 sh -c '
  echo "INPUT" \
    | PPZ_IPC_SOCKET="'"$PPZ_DAEMON_A_SOCK"'" ppz terminal share no-echo -- \
        sh -c "head -n 1 >/dev/null; printf OUTPUT"
' >/dev/null 2>/dev/null || true

wait_for 20 "ppz_a reread no-echo.stdout --json | jq -r '.payload' | tr -d '\n' | grep -q OUTPUT" >/dev/null

stdout=$(ppz_a reread no-echo.stdout --json | jq -r '.payload' | tr -d '\n')
echo "stdout=$stdout"
if echo "$stdout" | matches INPUT; then
  echo "input-leaked=yes"
else
  echo "input-leaked=no"
fi
