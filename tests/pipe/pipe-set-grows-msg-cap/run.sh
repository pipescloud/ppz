#!/usr/bin/env bash
# The mirror of the shrink case: a pipe created with a tight cap can be
# loosened afterwards, and the newly-raised cap actually retains more.
# Create with --max-msgs=2, publish 4 (only 2 survive), raise to 100,
# publish 4 more, and assert all 6 post-change messages are buffered.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null
ppz_a pipe create chat.ring --max-msgs=2 >/dev/null

for i in 1 2 3 4; do
  ppz_a send chat.ring "old-$i" >/dev/null
done
wait_for 20 "ppz_a reread chat.ring --json | jq -r '.payload' | grep -q old-4" >/dev/null

ppz_a pipe set chat.ring --max-msgs=100

for i in 5 6 7 8; do
  ppz_a send chat.ring "new-$i" >/dev/null
done
wait_for 20 "ppz_a reread chat.ring --json | jq -r '.payload' | grep -q new-8" >/dev/null

# 2 survivors of the tight cap + 4 new = 6 retained under the raised cap.
ppz_a ls | ls_normalize | awk '$1 == "chat.ring" {print "buffered="$3}'
