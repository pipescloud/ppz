#!/usr/bin/env bash
# Key revocation is scoped to the org in the URL: foo administers alpha,
# but naming beta's key id under /orgs/alpha/... must 404 and leave the
# key working. The old unauthenticated POST /api/v1/keys/<id>/revoke is
# gone entirely.
. /tests/lib/common.sh

beta_key_id="$(PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
  "SELECT k.id FROM api_keys k JOIN accounts a ON a.id = k.account_id
    WHERE a.name = 'beta' AND k.revoked_at IS NULL ORDER BY k.created_at LIMIT 1")"
ALPHA_ID=$(cat /seed/org-alpha.txt)

echo "anonymous api revoke: $(curl -sS "$PPZ_SERVER_URL/api/v1/keys/$beta_key_id/revoke" -X POST \
  -o /dev/null -w '%{http_code}')"

auth_as_foo
echo "cross-org revoke: $(curl_server "/orgs/$ALPHA_ID/keys/$beta_key_id/revoke" -X POST \
  -o /dev/null -w '%{http_code}')"

echo "beta key revoked: $(PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
  "SELECT revoked_at IS NOT NULL FROM api_keys WHERE id = '$beta_key_id'")"
