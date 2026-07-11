#!/usr/bin/env bash
# History flood cap: /chat/messages (and the WS history replay it shares code
# with) must NOT dump a window's entire backlog — a busy pipe would otherwise
# stream thousands of messages and one GetMsg round-trip each on every open.
# The drain is bounded to the most-recent N (tail-N, N=200), matching the
# read-flood cap the CLI already applies.
#
# Publish 210 numbered messages into a pipe, then read the window back: exactly
# 200 come through, the newest (msg-210) is present, and everything older than
# the cap (msg-001 .. msg-010) is gone.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a pipe create flood >/dev/null

for i in $(seq 1 210); do
  ppz_a send flood "msg-$(printf '%03d' "$i")" >/dev/null
done
wait_for 30 "curl_server '/orgs/alpha/chat/messages?kind=pipe&target=flood' | jq -e '.messages | length >= 200' >/dev/null"

MSGS=$(curl_server "/orgs/alpha/chat/messages?kind=pipe&target=flood")

echo "count: $(echo "$MSGS" | jq '.messages | length')"
echo "newest: $(echo "$MSGS" | jq -r '.messages[-1].payload')"
echo "oldest-kept: $(echo "$MSGS" | jq -r '.messages[0].payload')"
echo "msg-001-dropped: $(echo "$MSGS" | jq -r '[.messages[].payload] | index("msg-001") == null')"
