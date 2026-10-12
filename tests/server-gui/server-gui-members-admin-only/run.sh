#!/usr/bin/env bash
# Adding/removing org members is org administration: a plain member gets
# 403 and the membership is unchanged. Seed: foo owns alpha, bar is a
# non-owner member of alpha.
. /tests/lib/common.sh

ORG_ID=$(cat /seed/org-alpha.txt)
foo_id="$(PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
  "SELECT id FROM users WHERE username = 'foo'")"
bar_id="$(PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
  "SELECT id FROM users WHERE username = 'bar'")"
members() {
  PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
    "SELECT COUNT(*) FROM account_members WHERE account_id = '$ORG_ID'"
}
before="$(members)"

auth_as_bar
echo "member add: $(curl_server "/orgs/$ORG_ID/members" -X POST \
  --data-urlencode "user_id=$foo_id" -o /dev/null -w '%{http_code}')"
echo "member remove: $(curl_server "/orgs/$ORG_ID/members/$bar_id/remove" -X POST \
  -o /dev/null -w '%{http_code}')"
[[ "$(members)" == "$before" ]] && echo "membership unchanged" || echo "membership CHANGED"
