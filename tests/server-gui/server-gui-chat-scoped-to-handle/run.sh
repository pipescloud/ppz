#!/usr/bin/env bash
# Identity scoping (TUI parity: `ppz chat` runs AS a handle you set beforehand,
# and excludes your own handle from the roster). The web picks the acting handle
# up front (?as=, defaulting to your first) and drops it from the INBOXES so you
# can't DM yourself — and the exclusion is relative to whoever you're acting as.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create desk >/dev/null
ppz_a source create ops >/dev/null

# Acting as desk → desk is excluded, ops remains.
echo "--- as desk ---"
curl_server "/orgs/alpha/chat?as=desk" | grep -oE 'data-chat-entry="inbox:[^"]+"' | sort
# Acting as ops → ops is excluded, desk remains (exclusion is identity-relative).
echo "--- as ops ---"
curl_server "/orgs/alpha/chat?as=ops" | grep -oE 'data-chat-entry="inbox:[^"]+"' | sort
# The top-bar switcher reflects who you're acting as.
echo "--- selected ---"
curl_server "/orgs/alpha/chat?as=ops" | grep -oE '<span class="chat-picker-current">ops</span>'
