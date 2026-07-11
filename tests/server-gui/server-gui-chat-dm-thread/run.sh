#!/usr/bin/env bash
# DM reply-fanout (TUI participant-thread parity). Acting as handle `desk` and
# opening `ops`, the window shows only the two-way desk↔ops conversation —
# desk's messages to ops (from ops.inbox) stitched with ops's replies to desk
# (from desk.inbox) — and NOT a third party's messages to ops.
#
# foo owns desk, ops and snoop (created via key-alpha), so all three sends go
# through the web /chat/send with the sender stamped by `as`.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create ops >/dev/null
ppz_a source create desk >/dev/null
ppz_a source create snoop >/dev/null

send() { # as target payload
  curl_server "/orgs/alpha/chat/send" -X POST -H 'Content-Type: application/json' \
    -d "{\"kind\":\"source\",\"target\":\"$2\",\"payload\":\"$3\",\"as\":\"$1\"}" >/dev/null
}
send desk  ops  to-ops-from-desk   # my message to ops   -> ops.inbox
send ops   desk reply-from-ops     # ops's reply to me   -> desk.inbox
send snoop ops  noise-from-snoop   # third-party noise   -> ops.inbox (excluded)

wait_for 20 "curl_server '/orgs/alpha/chat/messages?kind=source&target=ops&as=desk' | jq -e '.messages | length >= 2' >/dev/null"

curl_server "/orgs/alpha/chat/messages?kind=source&target=ops&as=desk" \
  | jq -r '.messages[] | "\(.sender)|\(.payload)|\(.you)"' | sort
