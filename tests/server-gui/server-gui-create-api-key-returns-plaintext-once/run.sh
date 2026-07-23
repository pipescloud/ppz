#!/usr/bin/env bash
. /tests/lib/common.sh
auth_as_foo
# POST a new key — response page must include data-new-key="<plaintext>" exactly
# once. A subsequent GET /orgs/<id> must NOT contain that plaintext anywhere.
org_id="$(cat /seed/org-alpha.txt)"

resp="$(curl_server "/orgs/$org_id/keys" -X POST --data-urlencode 'label=test-key')"

plaintext="$(printf '%s' "$resp" | grep -oE 'data-new-key="[^"]+"' | sed -E 's/data-new-key="([^"]+)"/\1/')"

# Assert one occurrence in the response.
echo "response-occurrences=$(printf '%s' "$resp" | grep -oE 'data-new-key="[^"]+"' | wc -l | tr -d ' ')"

# Assert plaintext NOT in subsequent keys-tab page (the only place
# keys get listed, so the strongest negative check).
followup="$(curl_server "/orgs/$org_id/keys")"
if [[ -n "$plaintext" ]] && printf '%s' "$followup" | matches -F "$plaintext"; then
  echo "leaked-plaintext=true"
else
  echo "leaked-plaintext=false"
fi
