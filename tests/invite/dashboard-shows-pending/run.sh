#!/usr/bin/env bash
# alice has a pending invite to alpha. Her /dashboard renders it.
. /tests/lib/common.sh

# Invitee must exist in users for /dev/login to succeed (Phase 4
# allows inviting non-existent usernames at the API level — but the
# dashboard test exercises a logged-in invitee).
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -v ON_ERROR_STOP=1 -tAc "
  INSERT INTO users (id, username, email, mode)
  VALUES ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa','alice','alice@local','internal')
  ON CONFLICT (username) DO NOTHING
" >/dev/null

auth_as foo
curl_server "/orgs/alpha/invites" -X POST -d "username=alice" -o /dev/null

# Now sign in as alice and fetch the dashboard.
auth_as alice
body=$(curl_server "/dashboard" -s)

echo "invites_section_present: $(echo "$body" | matches 'Invitations' && echo true || echo false)"
echo "alpha_invite_present:    $(echo "$body" | matches 'Pending invitation to <strong>alpha' && echo true || echo false)"
echo "accept_form_present:     $(echo "$body" | matches 'action="/invites/.*/accept"' && echo true || echo false)"
echo "decline_form_present:    $(echo "$body" | matches 'action="/invites/.*/decline"' && echo true || echo false)"
