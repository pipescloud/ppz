#!/usr/bin/env bash
# After the user clicks "Approve as me", the POST handler 303s to
#   /oauth/device/verify?user_code=XXXX-XXXX&approved=1
# The GET handler must read `approved` and render the success message
# instead of the approval form. Bug from real life: handler dropped
# the query param, so the page rendered the form again — user clicks
# Approve, page looks identical, no feedback.
. /tests/lib/common.sh

# Route is gated by requireSession — establish a logged-in cookie first.
# We don't need a real device-code row in the DB to assert the page
# rendering — the GET handler is purely a template render driven by
# query params. Use a placeholder user_code to keep the test focused
# on the "approved=1" branch alone.
COOKIE_JAR=$(mktemp)
trap "rm -f $COOKIE_JAR" EXIT
curl_server "/dev/login?user=foo" -X POST -c "$COOKIE_JAR" -o /dev/null -s

echo "--- GET /oauth/device/verify (no approved flag) → renders the form ---"
body=$(curl_server "/oauth/device/verify?user_code=TEST-CODE" -b "$COOKIE_JAR" -s)
echo "form_button_present: $(echo "$body" | matches 'Approve as me' && echo true || echo false)"
echo "approved_message_present: $(echo "$body" | matches "You're all set" && echo true || echo false)"

echo ""
echo "--- GET /oauth/device/verify?approved=1 → renders the success message ---"
body=$(curl_server "/oauth/device/verify?user_code=TEST-CODE&approved=1" -b "$COOKIE_JAR" -s)
echo "form_button_present: $(echo "$body" | matches 'Approve as me' && echo true || echo false)"
echo "approved_message_present: $(echo "$body" | matches "You're all set" && echo true || echo false)"
