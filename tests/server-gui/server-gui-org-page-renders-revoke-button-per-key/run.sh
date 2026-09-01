#!/usr/bin/env bash
# The org page renders a Revoke button next to each ACTIVE key, and
# marks revoked keys with `data-key-state="revoked"` (CSS handles the
# strikethrough). Revoked keys do NOT get a button — once revoked,
# they're audit-trail rows.
#
# Lets operators see and revoke keys from the GUI without dropping
# into curl.
#
# The form posts to the OWNER-GATED /orgs/{id}/keys/{kid}/revoke, not the
# unauthenticated /api/v1/keys/{id}/revoke it used to target. A route with
# no caller has no actor, and key.revoke is the audit row you would least
# want to read "revoked by nobody".
. /tests/lib/common.sh
auth_as_foo

org_id="$(cat /seed/org-alpha.txt)"

# Issue two keys. One stays active; the other we revoke immediately.
resp_a="$(curl_server "/orgs/$org_id/keys" -X POST --data-urlencode 'label=stay-active')"
resp_b="$(curl_server "/orgs/$org_id/keys" -X POST --data-urlencode 'label=will-be-revoked')"
id_a="$(printf '%s' "$resp_a" | grep -oE 'data-key-id="[^"]+"' | sed -E 's/data-key-id="([^"]+)"/\1/')"
id_b="$(printf '%s' "$resp_b" | grep -oE 'data-key-id="[^"]+"' | sed -E 's/data-key-id="([^"]+)"/\1/')"

curl_server "/orgs/$org_id/keys/$id_b/revoke" -X POST -o /dev/null -w '' || true

# Revoke buttons live on the API-keys tab.
page="$(curl_server "/orgs/$org_id/keys")"

echo "--- active key has a revoke form pointing at its id ---"
if printf '%s' "$page" | matches -F "action=\"/orgs/$org_id/keys/$id_a/revoke\""; then
  echo "active-revoke-form=present"
else
  echo "active-revoke-form=missing"
fi

echo "--- revoked key has NO revoke form ---"
if printf '%s' "$page" | matches -F "action=\"/orgs/$org_id/keys/$id_b/revoke\""; then
  echo "revoked-revoke-form=present (BUG)"
else
  echo "revoked-revoke-form=absent"
fi

echo "--- revoked key is marked with data-key-state=revoked ---"
# `matches`, not `grep -q`: a short-circuiting grep would SIGPIPE the
# upstream tr (which streams the whole page) and flip this assertion to
# a false miss on a large page. See matches in common.sh.
if printf '%s' "$page" | tr '\n' ' ' \
   | matches -E "data-key-state=\"revoked\"[^>]*>[^<]*will-be-revoked|data-key-id=\"$id_b\"[^>]*data-key-state=\"revoked\""; then
  echo "revoked-state-marker=present"
else
  echo "revoked-state-marker=missing"
fi
