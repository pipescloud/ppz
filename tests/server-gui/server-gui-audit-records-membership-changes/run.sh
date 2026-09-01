#!/usr/bin/env bash
# Who is in the org, and at what tier, is the other half of "who can act
# as us" — the half the key trail can't answer. Adding a member, changing
# their role, and removing them are all recorded.
#
# member.role is included deliberately even though it wasn't a create or
# a destroy: promotion to admin hands someone the key, membership and
# ACL-enforcement gates in one POST, so it is the single most
# consequential membership edit and the one a reviewer will look for.
#
# Targets name the USER, not the membership row's uuid — a trail you have
# to join against `users` to read is a trail nobody reads.
#
# Setup (provided by the seed): foo owns alpha.
. /tests/lib/common.sh
auth_as_foo
ORG_ID=$(cat /seed/org-alpha.txt)

curl_server "/users" -X POST \
  --data-urlencode 'username=bob' \
  --data-urlencode 'email=bob@example.com' \
  --data-urlencode 'mode=internal' >/dev/null
bob_id="$(PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
  "SELECT id FROM users WHERE username = 'bob'")"

curl_server "/orgs/$ORG_ID/members" -X POST --data-urlencode "user_id=$bob_id" -o /dev/null
curl_server "/orgs/$ORG_ID/members/$bob_id/role" -X POST -d "role=admin" -o /dev/null
curl_server "/orgs/$ORG_ID/members/$bob_id/remove" -X POST -o /dev/null

echo "--- membership actions, newest first ---"
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-action="[^"]+"' \
  | sed -E 's/data-audit-action="([^"]+)"/\1/'

echo "--- targets name the user ---"
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-target="[^"]+"' \
  | sed -E 's/data-audit-target="([^"]+)"/\1/' \
  | sort -u

echo "--- deltas, newest first ---"
curl_server "/orgs/$ORG_ID/audit" -s \
  | grep -oE 'data-audit-delta="[^"]+"' \
  | sed -E 's/data-audit-delta="([^"]+)"/\1/'

echo "--- attributed to foo, acting in the GUI ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT u.username, (e.actor_api_key_id IS NULL)
    FROM audit_events e JOIN users u ON u.id = e.actor_user_id
   WHERE e.action = 'member.role'
" | sed 's/|/ /g'
