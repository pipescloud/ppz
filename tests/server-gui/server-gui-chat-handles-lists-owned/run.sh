#!/usr/bin/env bash
# The chat page's "send as" handle picker lists ONLY message handles the
# logged-in user created. foo owns `desk` (via key-alpha); bar owns `barhandle`
# (via key-alpha2) in the same org. foo's picker must show `desk` and must NOT
# show `barhandle`.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create desk >/dev/null

ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null
ppz_b source create barhandle >/dev/null
wait_for 20 "ppz_b ls | grep -q barhandle" >/dev/null

curl_server "/orgs/alpha/chat" \
  | grep -oE 'data-handle="[^"]+"' \
  | sed -E 's/data-handle="([^"]+)"/\1/' \
  | sort
