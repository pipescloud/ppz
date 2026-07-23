#!/usr/bin/env bash
# After approval, the success page should be a hero-sized confirmation
# (mirrors Claude Code's "Build something great / You're all set up"
# screen) rather than the existing terse one-liner. We assert two
# concrete shape signals:
#   - an <h1> element on the success branch (it's a moment, not a notice)
#   - the "you can close this window" copy that gives the user clear
#     next-step guidance (the previous "return to your terminal"
#     wording leaves them wondering if anything else is needed)
. /tests/lib/common.sh

COOKIE_JAR=$(mktemp)
trap "rm -f $COOKIE_JAR" EXIT
curl_server "/dev/login?user=foo" -X POST -c "$COOKIE_JAR" -o /dev/null -s

echo "--- form branch (no approved flag) → no success <h1> ---"
body=$(curl_server "/oauth/device/verify?user_code=TEST" -b "$COOKIE_JAR" -s)
echo "h1_present: $(echo "$body" | matches '<h1' && echo true || echo false)"

echo ""
echo "--- success branch (?approved=1) → renders hero <h1> + close-window copy ---"
body=$(curl_server "/oauth/device/verify?user_code=TEST&approved=1" -b "$COOKIE_JAR" -s)
echo "h1_present: $(echo "$body" | matches '<h1' && echo true || echo false)"
echo "close_window_copy: $(echo "$body" | matches -iE 'close this window' && echo true || echo false)"
