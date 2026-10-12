#!/usr/bin/env bash
# A plain member can revoke API keys they created (so a member who leaks
# a key can kill it) but not anyone else's, and the keys tab only offers
# Revoke where it would work. The users tab hides member add/remove from
# non-admins. Seed: foo owns alpha, bar is a non-owner member of alpha.
. /tests/lib/common.sh

ORG_ID=$(cat /seed/org-alpha.txt)
foo_key_id="$(PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
  "SELECT k.id FROM api_keys k JOIN users u ON u.id = k.created_by_user_id
    WHERE k.account_id = '$ORG_ID' AND u.username = 'foo' AND k.revoked_at IS NULL
    ORDER BY k.created_at LIMIT 1")"

auth_as_bar
resp="$(curl_server "/orgs/$ORG_ID/keys" -X POST --data-urlencode 'label=bar-key')"
bar_key_id="$(printf '%s' "$resp" | grep -oE 'data-key-id="[^"]+"' | sed -E 's/data-key-id="([^"]+)"/\1/')"

page="$(curl_server "/orgs/$ORG_ID/keys")"
form() { printf '%s' "$page" | matches -F "action=\"/orgs/$ORG_ID/keys/$1/revoke\"" && echo shown || echo hidden; }
echo "revoke button on own key: $(form "$bar_key_id")"
echo "revoke button on foo's key: $(form "$foo_key_id")"

users="$(curl_server "/orgs/$ORG_ID/users")"
printf '%s' "$users" | matches -F "action=\"/orgs/$ORG_ID/members\"" \
  && echo "add-member form: shown" || echo "add-member form: hidden"

echo "revoke foo's key: $(curl_server "/orgs/$ORG_ID/keys/$foo_key_id/revoke" -X POST -o /dev/null -w '%{http_code}')"
echo "revoke own key: $(curl_server "/orgs/$ORG_ID/keys/$bar_key_id/revoke" -X POST -o /dev/null -w '%{http_code}')"
echo "foo's key revoked: $(PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
  "SELECT revoked_at IS NOT NULL FROM api_keys WHERE id = '$foo_key_id'")"
echo "own key revoked: $(PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
  "SELECT revoked_at IS NOT NULL FROM api_keys WHERE id = '$bar_key_id'")"
