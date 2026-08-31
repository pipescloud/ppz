#!/usr/bin/env bash
# ACL changes belong in the audit trail (#191).
#
# An access-control change is arguably the edit most worth auditing: its
# entire purpose is to alter who can see what. docs/ACL.md listed this
# as a non-goal only because no trail existed when it was written.
#
# RED until acl.grant / acl.revoke / acl.enforce are recorded.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create ops >/dev/null
ppz_a pipe create ops.deploy-log >/dev/null

ppz_a pipe acl grant ops.deploy-log bar read >/dev/null
ppz_a acl enforce on >/dev/null
ppz_a pipe acl revoke ops.deploy-log bar read >/dev/null

echo "--- acl actions in the trail, oldest first ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT action, target FROM audit_events
   WHERE action LIKE 'acl.%'
   ORDER BY created_at ASC, action ASC
" | sed 's/|/ /'

echo "--- the grant records who and what ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT u.username, e.after::text
    FROM audit_events e JOIN users u ON u.id = e.actor_user_id
   WHERE e.action = 'acl.grant'
   LIMIT 1
" | sed 's/|/ /'
