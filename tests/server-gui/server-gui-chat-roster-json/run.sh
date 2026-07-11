#!/usr/bin/env bash
# Live roster refresh: GET /orgs/<slug>/chat/roster returns the same three
# sections as the page, as JSON, so the browser can re-poll agent liveness
# (dots/state) and the top-bar counts without a full reload — the web analog of
# the TUI's `who` poll. Here: one agent, one inbox, one pipe.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create ops >/dev/null
ppz_a pipe create general >/dev/null
ppz_a terminal share botty -- printf "hi" >/dev/null
wait_for 20 "curl_server '/orgs/alpha/chat/roster' | jq -e '.agents | length > 0' >/dev/null"

curl_server "/orgs/alpha/chat/roster" \
  | jq -r '"agents=\(.agents|length) inboxes=\(.inboxes|length) pipes=\(.pipes|length) online=\(.online)"'
curl_server "/orgs/alpha/chat/roster" | jq -r '.agents[].target'
