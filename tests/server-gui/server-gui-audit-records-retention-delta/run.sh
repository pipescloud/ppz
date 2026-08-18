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
# Matched on the attribute alone rather than a line window around the
# action. A window couples the assertion to how many lines the <tr>
# happens to span in org.html, so reformatting the template would break
# it for no behavioural reason. `pipe set` was the last action, and the
# tab renders newest-first (pinned by the lifecycle scenario), so the
# first delta on the page is its one.
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-delta="[^"]+"' \
  | sed -E 's/data-audit-delta="([^"]+)"/\1/' | head -1

echo "--- actor is attributed via the api key, not a bare username ---"
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-via="[^"]+"' \
  | sed -E 's/data-audit-via="([^"]+)"/\1/' | sort -u
