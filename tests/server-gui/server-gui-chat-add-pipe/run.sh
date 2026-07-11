#!/usr/bin/env bash
# Add-pipe from the web console (TUI parity: the `a` / [+ add pipe] affordance).
# POST /orgs/<slug>/chat/pipes {name} creates a root uncollared pipe + its
# stream; it then shows up in the roster's PIPES section as pipe:<name>, and is
# immediately followable (empty history, 200).
. /tests/lib/common.sh
auth_as_foo

echo "create: $(curl_server '/orgs/alpha/chat/pipes' -X POST -H 'Content-Type: application/json' \
  -d '{"name":"standup"}' -o /dev/null -w '%{http_code}')"

# Appears in the roster.
curl_server "/orgs/alpha/chat" | grep -oE 'data-chat-entry="pipe:standup"' | head -1

# And the window is live-followable (stream was provisioned) -> empty history.
echo "history: $(curl_server '/orgs/alpha/chat/messages?kind=pipe&target=standup' | jq -c '.messages')"
