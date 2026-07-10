#!/usr/bin/env bash
# GET /orgs/<slug>/chat/messages?kind=source&target=<handle> returns the
# buffered messages on that source's .inbox as JSON, newest last. A DM an
# agent sent from the CLI shows up on the web console. `you` is false —
# the message's sender isn't the web viewer (foo).
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create ops >/dev/null
ppz_a send ops.inbox "hello-inbox" >/dev/null
wait_for 20 "ppz_a ls | grep -q hello-inbox" >/dev/null

curl_server "/orgs/alpha/chat/messages?kind=source&target=ops" \
  | jq -r '.messages[] | "\(.payload)|\(.you)"'
