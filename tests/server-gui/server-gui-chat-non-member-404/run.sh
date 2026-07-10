#!/usr/bin/env bash
# Cross-tenant guard: /chat is a read+write surface (send publishes into the
# org's streams), so it gates on org membership. foo is a member of alpha
# only — every chat route under beta must 404 (not leak the org, not serve
# its roster/history, and above all not accept a send into it).
. /tests/lib/common.sh
auth_as_foo

echo "page: $(curl_server '/orgs/beta/chat' -o /dev/null -w '%{http_code}')"
echo "messages: $(curl_server '/orgs/beta/chat/messages?kind=pipe&target=general' -o /dev/null -w '%{http_code}')"
echo "send: $(curl_server '/orgs/beta/chat/send' -X POST -H 'Content-Type: application/json' -d '{"kind":"pipe","target":"general","payload":"intrusion"}' -o /dev/null -w '%{http_code}')"

# Sanity: foo's own org still works, proving the 404s above are the
# membership gate and not a blanket failure.
echo "own-page: $(curl_server '/orgs/alpha/chat' -o /dev/null -w '%{http_code}')"
