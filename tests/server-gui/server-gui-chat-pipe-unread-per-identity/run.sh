#!/usr/bin/env bash
# Pipe unread is per-identity (each handle is its own participant, like the
# TUI's per-handle chatstore). tester posts to pipe-x and reads it as tester;
# that must NOT clear the unread for inbox-a, who hasn't seen it.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create tester >/dev/null
ppz_a source create inbox-a >/dev/null
ppz_a pipe create pipe-x >/dev/null

# tester posts to pipe-x.
curl_server "/orgs/alpha/chat/send" -X POST -H 'Content-Type: application/json' \
  -d '{"kind":"pipe","target":"pipe-x","payload":"hello room","as":"tester"}' >/dev/null

PIPE_UNREAD='[.pipes[]|select(.target=="pipe-x")][0].unread'
wait_for 20 "curl_server '/orgs/alpha/chat/roster?as=inbox-a' | jq -e '(${PIPE_UNREAD}) == 1' >/dev/null"

# tester reads pipe-x (advances only tester's cursor).
curl_server "/orgs/alpha/chat/read" -X POST -H 'Content-Type: application/json' \
  -d '{"kind":"pipe","target":"pipe-x","as":"tester"}' >/dev/null

echo "tester: $(curl_server '/orgs/alpha/chat/roster?as=tester' | jq -r "$PIPE_UNREAD")"
echo "inbox-a: $(curl_server '/orgs/alpha/chat/roster?as=inbox-a' | jq -r "$PIPE_UNREAD")"
