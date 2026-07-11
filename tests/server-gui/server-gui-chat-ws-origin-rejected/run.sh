#!/usr/bin/env bash
# The chat WS enforces a same-origin check (CSWSH defense-in-depth): a handshake
# carrying a foreign Origin is rejected 403 before upgrading, even for a valid
# member session. (--max-time guards against the pre-fix behaviour, where the
# wildcard origin accepted the upgrade and the socket would hang.)
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a pipe create general >/dev/null

echo "foreign-origin: $(curl_server '/orgs/alpha/chat/ws?kind=pipe&target=general' \
  -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: AAAAAAAAAAAAAAAAAAAAAA==' \
  -H 'Origin: http://evil.example' \
  --max-time 4 -o /dev/null -w '%{http_code}')"
