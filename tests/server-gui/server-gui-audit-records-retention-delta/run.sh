#!/usr/bin/env bash
# A `pipe.set` row states what actually moved. The before/after payloads
# are stored as jsonb, so the tab can render "msgs 5000 → 5" rather than
# the useless "retention changed".
#
# Also pins the actor attribution caveat: the CLI acted through an API
# key, so the row names the key, not just the user it's attributed to.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null
ppz_a pipe create chat.archive >/dev/null
ppz_a pipe set chat.archive --max-msgs=5 >/dev/null

auth_as_foo
ORG_ID=$(cat /seed/org-alpha.txt)

echo "--- pipe.set delta ---"
# The <tr> spreads its data-* attributes over five lines; data-audit-delta
# is the last of them, so the window has to reach 4 lines past the action.
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -A 4 'data-audit-action="pipe.set"' \
  | grep -oE 'data-audit-delta="[^"]+"' \
  | sed -E 's/data-audit-delta="([^"]+)"/\1/' | head -1

echo "--- actor is attributed via the api key, not a bare username ---"
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-via="[^"]+"' \
  | sed -E 's/data-audit-via="([^"]+)"/\1/' | sort -u
