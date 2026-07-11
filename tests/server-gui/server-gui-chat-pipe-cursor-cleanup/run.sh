#!/usr/bin/env bash
# Removing a pipe must clear its read cursors, else recreating a same-name pipe
# (fresh stream from seq 1) inherits a stale high last_read_seq and its early
# messages show no unread badge. After delete+recreate+post, the new pipe's
# message is unread again.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create tester >/dev/null
ppz_a source create inbox-a >/dev/null
ppz_a pipe create zap >/dev/null

send() { curl_server "/orgs/alpha/chat/send" -X POST -H 'Content-Type: application/json' \
  -d "{\"kind\":\"pipe\",\"target\":\"zap\",\"payload\":\"$1\",\"as\":\"$2\"}" >/dev/null; }
ZAP_UNREAD='[.pipes[]|select(.target=="zap")][0].unread'

send "first" inbox-a
wait_for 20 "curl_server '/orgs/alpha/chat/roster?as=tester' | jq -e '(${ZAP_UNREAD}) == 1' >/dev/null"
# tester reads zap -> cursor at the current top seq.
curl_server "/orgs/alpha/chat/read" -X POST -H 'Content-Type: application/json' \
  -d '{"kind":"pipe","target":"zap","as":"tester"}' >/dev/null

# Delete + recreate the same-name pipe (fresh stream), then post again.
curl_server "/orgs/alpha/chat/pipes?target=zap" -X DELETE >/dev/null
ppz_a pipe create zap >/dev/null
send "after recreate" inbox-a
wait_for 20 "curl_server '/orgs/alpha/chat/messages?kind=pipe&target=zap' | jq -e '.messages | length > 0' >/dev/null"

# The recreated pipe's message must be unread for tester (stale cursor gone).
echo "unread-after-recreate: $(curl_server '/orgs/alpha/chat/roster?as=tester' | jq -r "$ZAP_UNREAD")"
