#!/usr/bin/env bash
# The chat roster classifies each org member into the right section:
#   - a pty source (terminal share)  -> AGENTS  (agent:<handle>)
#   - a message source (source create) -> INBOXES (inbox:<handle>)
#   - an uncollared pipe (pipe create)  -> PIPES   (pipe:<target>)
# Each row carries a stable data-chat-entry="<kind>:<target>" marker.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create ops >/dev/null
ppz_a source create me >/dev/null    # the viewer's own handle (excluded from its roster)
ppz_a pipe create general >/dev/null
# A pty source: `terminal share` provisions it; printf gives it a
# .stdout write so it's fully materialised. The bare source row is what
# the roster classifies as an agent.
ppz_a terminal share botty -- printf "hi" >/dev/null
wait_for 20 "ppz_a ls | grep -q botty" >/dev/null

# View as `me`, so `me` is excluded (no self-DM) and `ops` classifies as an inbox.
curl_server "/orgs/alpha/chat?as=me" \
  | grep -oE 'data-chat-entry="[^"]+"' \
  | sed -E 's/data-chat-entry="([^"]+)"/\1/' \
  | sort
