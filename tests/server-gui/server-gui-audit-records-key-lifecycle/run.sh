#!/usr/bin/env bash
# Minting and revoking an API key are the two operations that change who
# can act as the org. Both now land in the trail.
#
# This is also the first writer to produce `via web`. Every existing row
# comes from an API-key handler, so the audit tab's "web" rendering has
# been unreachable since it was written — key management is a GUI-only,
# session-authed flow, so it is the honest first case.
#
# The revoke button posts to the OWNER-GATED /orgs/{id}/keys/{kid}/revoke
# rather than the unauthenticated /api/v1/keys/{id}/revoke it used to
# target. A route with no caller has no actor, and an audit row that
# can't name who revoked the key is the one row you'd most want to trust.
#
# Setup (provided by the seed): foo owns alpha.
. /tests/lib/common.sh
auth_as_foo
ORG_ID=$(cat /seed/org-alpha.txt)

resp="$(curl_server "/orgs/$ORG_ID/keys" -X POST --data-urlencode 'label=ci-bot')"
kid="$(printf '%s' "$resp" | grep -oE 'data-key-id="[^"]+"' | sed -E 's/data-key-id="([^"]+)"/\1/')"

echo "--- the revoke form is owner-gated, not the anonymous api route ---"
page="$(curl_server "/orgs/$ORG_ID/keys")"
if printf '%s' "$page" | matches -F "action=\"/orgs/$ORG_ID/keys/$kid/revoke\""; then
  echo "revoke-form=owner-gated"
else
  echo "revoke-form=NOT owner-gated"
fi

curl_server "/orgs/$ORG_ID/keys/$kid/revoke" -X POST -o /dev/null

echo "--- audit actions, newest first ---"
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-action="[^"]+"' \
  | sed -E 's/data-audit-action="([^"]+)"/\1/'

echo "--- targets name the key by its label ---"
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-target="[^"]+"' \
  | sed -E 's/data-audit-target="([^"]+)"/\1/' \
  | sort -u

echo "--- a session action is attributed to the person, not a key ---"
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-via="[^"]+"' \
  | sed -E 's/data-audit-via="([^"]+)"/\1/' | sort -u
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-actor="[^"]+"' \
  | sed -E 's/data-audit-actor="([^"]+)"/\1/' | sort -u

echo "--- the revoke row states the state change ---"
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-delta="[^"]+"' \
  | sed -E 's/data-audit-delta="([^"]+)"/\1/' | head -1
