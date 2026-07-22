#!/usr/bin/env bash
# When the wrapped child sends a DA / cursor-position / focus query, the
# local terminal emits the *response* into our stdin. Some clients
# (shells with readline; broken TUIs) self-insert the response bytes as
# typed input, which then echoes back to master read and lands on .stdout —
# corrupting every viewer with sequences like ^[[?1;2c that the wrapped
# program never wrote.
#
# Probe: pipe a payload "BEFORE\033[?1;2cAFTER\n" into the wrapper. The
# child is `head -n 1` which echoes its stdin to stdout; the master
# read side then captures whatever made it through. With the filter,
# only "BEFOREAFTER" reaches the child (and thus .stdout). Without the
# filter, "?1;2c" leaks.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

timeout 15 sh -c '
  printf "BEFORE\033[?1;2cAFTER\n" \
    | PPZ_IPC_SOCKET="'"$PPZ_DAEMON_A_SOCK"'" ppz terminal share esc-filter -- head -n 1
' >/dev/null 2>/dev/null || true

wait_for 20 "ppz_a reread esc-filter.stdout --json | jq -r '.payload' | tr -d '\n' | grep -q AFTER" >/dev/null

stdout=$(ppz_a reread esc-filter.stdout --json | jq -r '.payload' | tr -d '\n\r')
echo "stdout=$stdout"
if echo "$stdout" | matches '?1;2c'; then
  echo "esc-leaked=yes"
else
  echo "esc-leaked=no"
fi
