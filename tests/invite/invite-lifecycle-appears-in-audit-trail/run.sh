#!/usr/bin/env bash
# Invites are how people actually join an org — POST /orgs/{id}/members is
# an owner-side shortcut, but the invite flow is the path a real member
# arrives by. A membership trail that only records the shortcut would show
# an org whose members mostly appeared from nowhere.
#
# The interesting asymmetry: invite.create and invite.revoke are the
# OWNER's actions, while invite.accept and invite.decline are the
# INVITEE's. All four are filed against the ORG's trail (that's whose
# membership changed) but attributed to whoever actually acted — so
# alice's acceptance names alice, not the foo who invited her.
#
# RED until the four invite actions are recorded.
. /tests/lib/common.sh
ORG_ID=$(cat /seed/org-alpha.txt)

auth_as_foo
for u in alice bob carol; do
  curl_server "/users" -X POST \
    --data-urlencode "username=$u" \
    --data-urlencode "email=$u@example.com" \
    --data-urlencode 'mode=internal' >/dev/null
  curl_server "/orgs/$ORG_ID/invites" -X POST --data-urlencode "username=$u" -o /dev/null
done

invite_of() {
  PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
    "SELECT id FROM invites WHERE invitee_username = '$1' AND status = 'pending' LIMIT 1"
}

carol_invite="$(invite_of carol)"
curl_server "/orgs/$ORG_ID/invites/$carol_invite/revoke" -X POST -o /dev/null

auth_as alice
curl_server "/invites/$(invite_of alice)/accept" -X POST -o /dev/null
auth_as bob
curl_server "/invites/$(invite_of bob)/decline" -X POST -o /dev/null

echo "--- invite actions with who acted, oldest first ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT e.action, e.target_type, e.target, u.username
    FROM audit_events e JOIN users u ON u.id = e.actor_user_id
   WHERE e.action LIKE 'invite.%'
   ORDER BY e.created_at ASC
" | sed 's/|/ /g'

echo "--- every row is filed against alpha's trail ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT count(*)::text FROM audit_events e JOIN accounts a ON a.id = e.account_id
   WHERE e.action LIKE 'invite.%' AND a.name = 'alpha'
"

echo "--- state transitions ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT action, before::text, after::text FROM audit_events
   WHERE action IN ('invite.accept','invite.revoke')
   ORDER BY action ASC
" | sed 's/|/ /g'
