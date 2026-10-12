#!/usr/bin/env bash
# The terminal WS streams a shared terminal's full output, so it is
# session-gated (anonymous → redirect to login, never an upgrade) and
# enforces same-origin like the chat WS (a member's browser visiting a
# foreign page can't be used to attach). --max-time guards against the
# pre-fix behaviour, where the upgrade was accepted and the socket hung.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a terminal share term1 -- printf "hello-from-share" >/dev/null
wait_for 20 "ppz_a read term1.stdout --json | jq -r '.payload' | grep -q hello-from-share" >/dev/null

ws() {
  curl -sS "$PPZ_SERVER_URL/orgs/alpha/sources/term1/terminal/ws" \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
    -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: AAAAAAAAAAAAAAAAAAAAAA==' \
    --max-time 4 -o /dev/null -w '%{http_code}' "$@"
}

echo "anonymous: $(ws)"
auth_as_foo
echo "member, foreign origin: $(ws -b "$PPZ_TEST_COOKIE_JAR" -H 'Origin: http://evil.example')"
