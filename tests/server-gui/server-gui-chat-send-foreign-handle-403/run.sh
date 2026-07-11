#!/usr/bin/env bash
# Handle-ownership invariant: you may only send AS a handle you created. foo
# (web session) tries to send stamped as `barhandle`, a message source created
# by bar (via key-alpha2, whose creator is bar) in the same org — the server
# must reject it 403, and nothing lands in the pipe.
#
# Also: sending as a handle that doesn't exist at all is likewise refused.
. /tests/lib/common.sh
auth_as_foo

# foo's own daemon (key-alpha) sets up the pipe.
ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a pipe create general >/dev/null

# bar's key (key-alpha2) creates a handle in alpha owned by bar, not foo.
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null
ppz_b source create barhandle >/dev/null
wait_for 20 "ppz_b ls | grep -q barhandle" >/dev/null

echo "foreign: $(curl_server '/orgs/alpha/chat/send' -X POST -H 'Content-Type: application/json' \
  -d '{"kind":"pipe","target":"general","payload":"intrusion","as":"barhandle"}' -o /dev/null -w '%{http_code}')"
echo "nonexistent: $(curl_server '/orgs/alpha/chat/send' -X POST -H 'Content-Type: application/json' \
  -d '{"kind":"pipe","target":"general","payload":"intrusion","as":"ghost"}' -o /dev/null -w '%{http_code}')"

# Nothing should have been published by the rejected sends.
echo "landed: $(curl_server '/orgs/alpha/chat/messages?kind=pipe&target=general' | jq '.messages | length')"
