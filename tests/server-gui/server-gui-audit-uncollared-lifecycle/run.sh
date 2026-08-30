#!/usr/bin/env bash
# UNCOLLARED (sourceless) pipes go through their own body-addressed
# endpoints — POST/PATCH/DELETE /api/v1/pipes — parallel to the collared
# /sources/{handle}/pipes ones. The audit trail must cover BOTH sets, or
# "all pipe lifecycle mutations are recorded" is only true of pipes that
# happen to hang off a source.
#
# Setup (provided by the seed): foo owns alpha.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

ppz_a pipe create room >/dev/null
ppz_a pipe set room --max-msgs=50 >/dev/null
ppz_a pipe destroy room >/dev/null

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
