#!/usr/bin/env bash
# POST /orgs/<slug>/chat/send publishes a message from the browser into
# the target window's stream, stamped with the viewer's username as
# sender. Here the web user (foo) posts into an uncollared pipe, then
# reads it back over /chat/messages — sender is foo and you is true.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a pipe create general >/dev/null

curl_server "/orgs/alpha/chat/send" \
  -X POST -H 'Content-Type: application/json' \
  -d '{"kind":"pipe","target":"general","payload":"hello-web"}' >/dev/null

wait_for 20 "curl_server '/orgs/alpha/chat/messages?kind=pipe&target=general' | jq -e '.messages | length > 0' >/dev/null"

curl_server "/orgs/alpha/chat/messages?kind=pipe&target=general" \
  | jq -r '.messages[] | "\(.sender)|\(.payload)|\(.you)"'
