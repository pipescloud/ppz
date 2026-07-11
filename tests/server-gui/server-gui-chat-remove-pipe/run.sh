#!/usr/bin/env bash
# Remove-pipe from the web console (TUI parity: the `-` key). DELETE
# /orgs/<slug>/chat/pipes?target=<name> removes the uncollared pipe row + its
# stream; it then disappears from the roster. A second delete is 404.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a pipe create doomed >/dev/null
wait_for 20 "curl_server '/orgs/alpha/chat' | grep -q 'pipe:doomed'"

echo "before: $(curl_server '/orgs/alpha/chat' | grep -c 'data-chat-entry=\"pipe:doomed\"')"
echo "delete: $(curl_server '/orgs/alpha/chat/pipes?target=doomed' -X DELETE -o /dev/null -w '%{http_code}')"
echo "after: $(curl_server '/orgs/alpha/chat' | grep -c 'data-chat-entry=\"pipe:doomed\"')"
echo "delete-again: $(curl_server '/orgs/alpha/chat/pipes?target=doomed' -X DELETE -o /dev/null -w '%{http_code}')"
