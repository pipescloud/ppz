#!/usr/bin/env bash
# DM unread is per-conversation and counts the counterparty's messages in MY
# inbox — not the window's own stream. crux messages desk; viewing as desk, the
# `crux` DM shows 1 unread (crux's message landed in desk.inbox). Marking it read
# (advancing this conversation's cursor to my inbox's newest seq) clears it.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create crux >/dev/null
ppz_a source create desk >/dev/null

# crux -> desk.inbox (crux messages desk).
curl_server "/orgs/alpha/chat/send" -X POST -H 'Content-Type: application/json' \
  -d '{"kind":"source","target":"desk","payload":"hi desk from crux","as":"crux"}' >/dev/null

CRUX_UNREAD='[.inboxes[]|select(.target=="crux")][0].unread'
wait_for 20 "curl_server '/orgs/alpha/chat/roster?as=desk' | jq -e '(${CRUX_UNREAD}) == 1' >/dev/null"

echo "before: $(curl_server '/orgs/alpha/chat/roster?as=desk' | jq -r "$CRUX_UNREAD")"
echo "read: $(curl_server '/orgs/alpha/chat/read' -X POST -H 'Content-Type: application/json' \
  -d '{"kind":"source","target":"crux","as":"desk"}' -o /dev/null -w '%{http_code}')"
echo "after: $(curl_server '/orgs/alpha/chat/roster?as=desk' | jq -r "$CRUX_UNREAD")"
