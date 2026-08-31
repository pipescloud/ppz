#!/usr/bin/env bash
# The Security tab is org-admin territory, gated in the handler rather
# than the template — same reasoning as the audit tab: a non-admin
# should get a refusal, not a page with a silently empty section.
#
# It matters more here than for a read-only view: the rights table
# enumerates every principal's access across the org, and the toggle
# changes the org's security posture.
#
# Seed gives foo=owner of alpha, bar=member.
. /tests/lib/common.sh

org_id="$(cat /seed/org-alpha.txt)"

echo "--- owner may view the tab ---"
auth_as_foo
curl_server "/orgs/$org_id/security" -o /dev/null -w "status=%{http_code}\n"

echo "--- a plain member may not ---"
auth_as_bar
curl_server "/orgs/$org_id/security" -o /dev/null -w "status=%{http_code}\n"

echo "--- nor may they flip the switch ---"
curl_server "/orgs/$org_id/security/enforce" -X POST -d 'enforced=on' \
  --max-redirs 0 -o /dev/null -w "status=%{http_code}\n"

echo "--- and enforcement is untouched ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
  "SELECT acl_enforced FROM accounts WHERE name = 'alpha'"

echo "--- the nav hides the tab from a member ---"
if curl_server "/orgs/$org_id/pipes" | matches -F 'data-tab="security"'; then
  echo "nav-link=shown"
else
  echo "nav-link=hidden"
fi
