#!/usr/bin/env bash
# ACL Phase 1: admin is deliberately BELOW owner. Widening the org
# gates to admin must not widen role-granting itself — otherwise any
# admin could promote themselves and the owner tier is decorative.
#
# foo promotes bar to admin; bar then tries to promote themselves to
# owner, and to demote foo. Both must be refused.
#
# RED until Phase 1 lands.
. /tests/lib/common.sh

ORG_ID=$(cat /seed/org-alpha.txt)
BAR_ID=$(cat /seed/user-bar.txt)
FOO_ID=$(cat /seed/user-foo.txt)

FOO_JAR=$(mktemp); BAR_JAR=$(mktemp)
trap "rm -f $FOO_JAR $BAR_JAR" EXIT
curl -sS "$PPZ_SERVER_URL/dev/login?user=foo" -X POST -c "$FOO_JAR" -o /dev/null
curl -sS "$PPZ_SERVER_URL/dev/login?user=bar" -X POST -c "$BAR_JAR" -o /dev/null

echo "--- foo (owner) promotes bar to admin ---"
curl -sS "$PPZ_SERVER_URL/orgs/$ORG_ID/members/$BAR_ID/role" -X POST -b "$FOO_JAR" \
  -d "role=admin" --max-redirs 0 -o /dev/null -w "status=%{http_code}\n"

echo "--- bar (admin) tries to promote themselves to owner ---"
curl -sS "$PPZ_SERVER_URL/orgs/$ORG_ID/members/$BAR_ID/role" -X POST -b "$BAR_JAR" \
  -d "role=owner" --max-redirs 0 -o /dev/null -w "status=%{http_code}\n"

echo "--- bar (admin) tries to demote foo ---"
curl -sS "$PPZ_SERVER_URL/orgs/$ORG_ID/members/$FOO_ID/role" -X POST -b "$BAR_JAR" \
  -d "role=member" --max-redirs 0 -o /dev/null -w "status=%{http_code}\n"

echo "--- alpha's owner is unchanged ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT u.username FROM accounts a JOIN users u ON u.id = a.owner_user_id
   WHERE a.name = 'alpha'"
