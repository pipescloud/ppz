#!/usr/bin/env bash
# ACL Phase 0a: an API key authenticates AS a principal.
#
# key-alpha and key-alpha2 are both keys on org alpha and differ only in
# who they act as — alpha-primary's principal is foo (alpha's owner),
# alpha-secondary's is bar (a member, not the owner). The owner-only
# invite gate therefore tells them apart, but only if a key resolves to
# a user at all.
#
# Today it doesn't: requireBearer leaves AuthedCaller.UserID as uuid.Nil
# on the API-key path (internal/server/auth_bearer.go), so both keys are
# rejected identically with "requires an OAuth token (user identity)"
# and there is no principal for an ACL grant to name.
#
# RED until Phase 0a lands.
. /tests/lib/common.sh

invite_as() {
  local key="$1" invitee="$2" label="$3" code
  code=$(curl -sS -o /tmp/invite-body.json -w '%{http_code}' \
    -X POST "$PPZ_SERVER_URL/api/v1/orgs/alpha/invites" \
    -H "Authorization: Bearer $key" \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"$invitee\"}")
  echo "$label status:$code $(jq -r '.error // .invite.invitee_username' /tmp/invite-body.json)"
}

# Acts as foo, alpha's owner → passes the owner gate.
invite_as "$(key_alpha)" carol foo-key

# Acts as bar, a member of alpha but not its owner → must be rejected by
# the owner gate, not by "you have no identity".
invite_as "$(key_alpha2)" dave bar-key

# The stored row attributes to the key's principal. Proves the identity
# reached the write path, not just the auth check.
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT i.invitee_username, u.username
    FROM invites i JOIN users u ON u.id = i.inviter_user_id
   ORDER BY i.invitee_username
" | sed 's/|/ /'
