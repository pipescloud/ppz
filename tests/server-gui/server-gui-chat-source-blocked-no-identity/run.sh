#!/usr/bin/env bash
# A source (agent/inbox) window is a DM: without an acting `?as=` identity the
# web console must NOT surface the handle's raw inbox (every sender's messages
# to it). The message is genuinely on ops.inbox (provable via `ppz ls`), yet
# GET /chat/messages?kind=source&target=ops with no `as` returns an empty
# thread — the window is blocked until the viewer picks a send-as identity.
# (Previously this path returned the whole inbox — the "god's-eye" leak.)
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create ops >/dev/null
ppz_a send ops.inbox "hello-inbox" >/dev/null
wait_for 20 "ppz_a ls | grep -q hello-inbox" >/dev/null

# The message really is on the inbox …
ppz_a ls | grep -q hello-inbox && echo "on-inbox: yes"
# … but the web read without an identity surfaces nothing.
echo "web-no-as: $(curl_server '/orgs/alpha/chat/messages?kind=source&target=ops' | jq '.messages | length')"
