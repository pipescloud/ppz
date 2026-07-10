#!/usr/bin/env bash
# The web `ppz chat` console at /orgs/<slug>/chat renders the three
# roster sections (Agents / Inboxes / Pipes), exposes the viewer's
# identity as data-me (the outbound sender), and loads the chat.js
# asset that drives the live pane. Page contract only — no data needed.
. /tests/lib/common.sh
auth_as_foo

PAGE=$(curl_server "/orgs/alpha/chat")

echo "--- sections ---"
echo "$PAGE" | grep -oE 'data-section="[a-z]+"' | sort -u

echo "--- me ---"
echo "$PAGE" | grep -oE 'data-me="[a-z]+"'

echo "--- chat.js ---"
echo "$PAGE" | grep -oE '/assets/chat\.js' | head -1
