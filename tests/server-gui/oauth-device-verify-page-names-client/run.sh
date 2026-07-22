#!/usr/bin/env bash
# Mirrors the Claude Code consent-screen pattern: instead of generic
# "A CLI session is requesting access", the verify page should name
# the specific client the user is approving — pulled from the
# `client_name` the CLI sends with /oauth/device/code.
. /tests/lib/common.sh

CLIENT="ppz CLI 0.15.0 (auth-test)"

# Throwaway logged-in user (the verify page is gated by requireSession).
COOKIE_JAR=$(mktemp)
trap "rm -f $COOKIE_JAR" EXIT
curl_server "/dev/login?user=foo" -X POST -c "$COOKIE_JAR" -o /dev/null -s

echo "--- POST /oauth/device/code with client_name ---"
mint_body=$(curl_server "/oauth/device/code" -X POST \
    -H "Content-Type: application/json" \
    -d "{\"client_name\":\"$CLIENT\"}" -s)
USER_CODE=$(echo "$mint_body" | grep -oE '"user_code":"[^"]+"' | sed -E 's/"user_code":"([^"]+)"/\1/')
echo "minted: $([[ -n "$USER_CODE" ]] && echo true || echo false)"

echo ""
echo "--- GET /oauth/device/verify → page names the client ---"
body=$(curl_server "/oauth/device/verify?user_code=$USER_CODE" -b "$COOKIE_JAR" -s)
echo "client_name_present: $(echo "$body" | matches -F "$CLIENT" && echo true || echo false)"
