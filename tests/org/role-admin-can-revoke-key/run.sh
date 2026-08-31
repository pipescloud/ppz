#!/usr/bin/env bash
# ACL Phase 1: the new `admin` org role can do the owner-gated things
# that a plain member cannot.
#
# Seed gives foo=owner of alpha, bar=member. foo promotes bar to admin,
# after which bar can revoke a key — the same action that returns 403
# for a member (covered by server-gui/auth-revoke-key-non-owner-403).
#
# RED until Phase 1 lands.
. /tests/lib/common.sh

ORG_ID=$(cat /seed/org-alpha.txt)
BAR_ID=$(cat /seed/user-bar.txt)

FOO_JAR=$(mktemp); BAR_JAR=$(mktemp)
trap "rm -f $FOO_JAR $BAR_JAR" EXIT
curl -sS "$PPZ_SERVER_URL/dev/login?user=foo" -X POST -c "$FOO_JAR" -o /dev/null
curl -sS "$PPZ_SERVER_URL/dev/login?user=bar" -X POST -c "$BAR_JAR" -o /dev/null

echo "--- foo (owner) promotes bar to admin ---"
curl -sS "$PPZ_SERVER_URL/orgs/$ORG_ID/members/$BAR_ID/role" -X POST -b "$FOO_JAR" \
  -d "role=admin" --max-redirs 0 -o /dev/null -w "status=%{http_code}\n"

KEY_ID=$(curl -sS "$PPZ_SERVER_URL/orgs/$ORG_ID/keys" -b "$FOO_JAR" \
           | grep -oE 'data-key-id="[^"]+"' | head -1 \
           | sed -E 's/data-key-id="([^"]+)"/\1/')

echo "--- bar (now admin) revokes a key ---"
curl -sS "$PPZ_SERVER_URL/orgs/$ORG_ID/keys/$KEY_ID/revoke" -X POST -b "$BAR_JAR" \
  --max-redirs 0 -o /dev/null -w "status=%{http_code}\n"

echo "--- key state after the revoke ---"
curl -sS "$PPZ_SERVER_URL/orgs/$ORG_ID/keys" -b "$FOO_JAR" \
  | grep -A 1 "data-key-id=\"$KEY_ID\"" \
  | grep -oE 'data-key-state="[^"]+"' | head -1
