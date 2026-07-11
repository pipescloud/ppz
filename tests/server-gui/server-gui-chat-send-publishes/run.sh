#!/usr/bin/env bash
# POST /orgs/<slug>/chat/send publishes from the browser stamped with the
# viewer's chosen HANDLE (the `as` field) as sender — not their account
# username. The web user acts as one of their own message handles, mirroring
# the CLI's current handle. Here foo owns `desk` (created via key-alpha, whose
# creator is foo), sends into a pipe as `desk`, and reads it back: sender is
# `desk` and — reading as `desk` — `you` is true.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a pipe create general >/dev/null
ppz_a source create desk >/dev/null

curl_server "/orgs/alpha/chat/send" \
  -X POST -H 'Content-Type: application/json' \
  -d '{"kind":"pipe","target":"general","payload":"hello-web","as":"desk"}' >/dev/null

wait_for 20 "curl_server '/orgs/alpha/chat/messages?kind=pipe&target=general&as=desk' | jq -e '.messages | length > 0' >/dev/null"

curl_server "/orgs/alpha/chat/messages?kind=pipe&target=general&as=desk" \
  | jq -r '.messages[] | "\(.sender)|\(.payload)|\(.you)"'
