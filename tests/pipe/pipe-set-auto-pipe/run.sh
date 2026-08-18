#!/usr/bin/env bash
# Auto-provisioned pipes (inbox, and stdin/stdout/… on pty sources) are
# JetStream-only — they have no row in the `pipes` table, which is
# exactly why retention was previously unreachable for them. They are
# also the pipes whose default caps users hit first.
#
# `pipe set` materialises a pipes row on first override for these names,
# bypassing the reserved-name gate that `pipe create` enforces. Two
# things must hold afterwards: the cap takes effect, and `ppz ls` still
# lists the pipe exactly once (readers union auto-pipes with table rows,
# so a materialised row must not double up).
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

# `pipe create chat.inbox` is refused — inbox is reserved.
ppz_a pipe create chat.inbox >/dev/null 2>&1 || echo "create refused (reserved), as expected"

ppz_a pipe set chat.inbox --max-msgs=3

for i in 1 2 3 4 5; do
  ppz_a send chat.inbox "msg-$i" >/dev/null
done
wait_for 20 "ppz_a reread chat.inbox --json | jq -r '.payload' | grep -q msg-5" >/dev/null

ppz_a ls | ls_normalize | awk '$1 == "chat.inbox" {print "buffered="$3}'
echo "rows=$(ppz_a ls | ls_normalize | awk '$1 == "chat.inbox"' | wc -l | tr -d ' ')"
