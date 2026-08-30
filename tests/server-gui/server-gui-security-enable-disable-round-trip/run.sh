#!/usr/bin/env bash
# Enabling and disabling enforcement from the Security tab.
#
# The round trip is the point: disabling is non-destructive, so grants
# survive it and re-enabling restores exactly the previous configuration
# rather than resetting to defaults. An admin who flips this off to
# unblock someone must not silently lose the access they had configured.
. /tests/lib/common.sh
auth_as_foo

org_id="$(cat /seed/org-alpha.txt)"

curl_server "/api/v1/sources" -X POST -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $(key_alpha)" -d '{"handle":"ops","kind":"message"}' -o /dev/null
curl_server "/api/v1/sources/ops/pipes" -X POST -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $(key_alpha)" -d '{"name":"deploy-log"}' -o /dev/null
# One explicit grant, so the rights table has provenance other than defaults.
curl_server "/api/v1/acl/grant" -X POST -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $(key_alpha)" \
  -d '{"pipe":"ops.deploy-log","principal":"bar","perm":"read"}' -o /dev/null

echo "--- enable from the GUI form ---"
curl_server "/orgs/$org_id/security/enforce" -X POST -d 'enforced=on' \
  --max-redirs 0 -o /dev/null -w "status=%{http_code}\n"

page="$(curl_server "/orgs/$org_id/security")"
echo "--- the tab now reports enforced and renders the rights table ---"
printf '%s' "$page" | matches -F 'data-acl-state="enforced"' && echo "state=enforced" || echo "state=WRONG"
printf '%s' "$page" | matches -F 'id="acl-rights"' && echo "rights-table=present" || echo "rights-table=missing"

echo "--- bar's granted row is labelled as a grant, not a default ---"
printf '%s' "$page" | tr '\n' ' ' \
  | matches -E 'data-principal="bar" data-pipe="ops.deploy-log">.*grant' \
  && echo "grant-provenance=present" || echo "grant-provenance=missing"

echo "--- disable again ---"
curl_server "/orgs/$org_id/security/enforce" -X POST -d 'enforced=false' \
  --max-redirs 0 -o /dev/null -w "status=%{http_code}\n"

echo "--- back to off, and the grant survived ---"
curl_server "/orgs/$org_id/security" | matches -F 'data-acl-state="off"' && echo "state=off" || echo "state=WRONG"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
  "SELECT count(*) FROM acl_grants WHERE selector = 'ops.deploy-log'"
