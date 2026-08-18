#!/usr/bin/env bash
# `ppz pipe set LEAF` addresses an uncollared pipe at the session's
# current namespace — the same Phase 1.5.1 target grammar `pipe create`
# and `pipe destroy` use. Current handle plays no part in routing.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a unset handle >/dev/null 2>&1
ppz_a unset namespace >/dev/null 2>&1

# Source foo exists (so current_handle=foo) and a namespace is set —
# the set must land on red-team.room, not foo.room.
ppz_a source create foo >/dev/null
ppz_a set namespace red-team >/dev/null
ppz_a pipe create room >/dev/null

ppz_a pipe set room --max-msgs=5

for i in 1 2 3 4 5 6 7; do
  ppz_a send room "msg-$i" >/dev/null
done
wait_for 20 "ppz_a reread room --json | jq -r '.payload' | grep -q msg-7" >/dev/null

ppz_a ls | ls_normalize | awk '$1 == "red-team.room" {print "buffered="$3}'
