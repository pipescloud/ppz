#!/usr/bin/env bash
# Multi-org login: the device-flow verify page must let the user choose
# which org the CLI session authorizes into, instead of silently using a
# server-picked default.
#
# bar belongs to alpha + beta and owns neither, so the dropdown lists both
# and defaults to bar's default org — the oldest one they belong to
# (alpha, seeded before beta).
. /tests/lib/common.sh

COOKIE_JAR=$(mktemp)
trap "rm -f $COOKIE_JAR" EXIT
curl_server "/dev/login?user=bar" -X POST -c "$COOKIE_JAR" -o /dev/null -s

mint=$(curl_server "/oauth/device/code" -X POST -H "Content-Type: application/json" -d '{}' -s)
USER_CODE=$(echo "$mint" | grep -oE '"user_code":"[^"]+"' | sed -E 's/.*:"([^"]+)"/\1/')

page=$(curl_server "/oauth/device/verify?user_code=$USER_CODE" -b "$COOKIE_JAR" -s)

echo "has_select: $(echo "$page" | matches -E '<select[^>]*name="account_id"' && echo true || echo false)"
echo "has_alpha: $(echo "$page" | matches -E '<option[^>]*>alpha</option>' && echo true || echo false)"
echo "has_beta: $(echo "$page" | matches -E '<option[^>]*>beta</option>' && echo true || echo false)"
echo "default_selected: $(echo "$page" | grep -oE '<option value="[^"]*" selected>[^<]+</option>' | sed -E 's/.*>([^<]+)<.*/\1/')"
