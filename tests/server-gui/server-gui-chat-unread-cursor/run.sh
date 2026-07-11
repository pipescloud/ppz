#!/usr/bin/env bash
# Unread badges via a per-user read cursor. A window's unread count is
# stream.LastSeq minus the viewer's cursor. With a fresh cursor (never opened),
# two messages in a pipe show unread=2; POST /chat/read advances the cursor to
# the newest sequence, so unread drops to 0.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a pipe create buzz >/dev/null
ppz_a send buzz "one" >/dev/null
ppz_a send buzz "two" >/dev/null
wait_for 20 "curl_server '/orgs/alpha/chat/roster' | jq -e '[.pipes[]|select(.target==\"buzz\")][0].unread == 2' >/dev/null"

echo "before: $(curl_server '/orgs/alpha/chat/roster' | jq -r '[.pipes[]|select(.target=="buzz")][0].unread')"
echo "read: $(curl_server '/orgs/alpha/chat/read' -X POST -H 'Content-Type: application/json' \
  -d '{"kind":"pipe","target":"buzz"}' -o /dev/null -w '%{http_code}')"
echo "after: $(curl_server '/orgs/alpha/chat/roster' | jq -r '[.pipes[]|select(.target=="buzz")][0].unread')"
