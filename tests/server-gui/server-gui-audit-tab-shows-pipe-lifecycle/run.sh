#!/usr/bin/env bash
# The audit tab records all three pipe lifecycle mutations — create,
# set, destroy — newest first. This covers the collared endpoints; the
# uncollared ones have their own scenario.
#
# Everything here goes through the CLI, so it's attributed `via api-key`;
# the Pipes-tab path (`via web`) is covered by
# server-gui-pipes-manage-lifecycle. `ppz source create` is recorded too,
# as source.create.
#
# Setup (provided by the seed): foo owns alpha.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

ppz_a pipe create chat.archive >/dev/null
ppz_a pipe set chat.archive --max-msgs=50 >/dev/null
ppz_a pipe destroy chat.archive >/dev/null

auth_as_foo
ORG_ID=$(cat /seed/org-alpha.txt)

echo "--- audit actions, newest first ---"
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-action="[^"]+"' \
  | sed -E 's/data-audit-action="([^"]+)"/\1/'

echo "--- targets ---"
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-target="[^"]+"' \
  | sed -E 's/data-audit-target="([^"]+)"/\1/' \
  | sort -u
