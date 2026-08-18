#!/usr/bin/env bash
# Owner-only gate on the audit tab, matching the existing key-revoke
# gate. A non-owner member can see the org's pipes but not who changed
# what — the trail names people and the keys they used.
#
# Setup (provided by the seed): foo owns alpha, bar is a non-owner
# member of alpha.
. /tests/lib/common.sh

ORG_ID=$(cat /seed/org-alpha.txt)

auth_as_bar
echo "--- GET audit tab as bar (member, not owner) ---"
curl_server "/orgs/$ORG_ID/audit" -o /dev/null -w "status=%{http_code}\n" -s

echo "--- bar can still reach the pipes tab ---"
curl_server "/orgs/$ORG_ID/pipes" -o /dev/null -w "status=%{http_code}\n" -s

auth_as_foo
echo "--- GET audit tab as foo (owner) ---"
curl_server "/orgs/$ORG_ID/audit" -o /dev/null -w "status=%{http_code}\n" -s
