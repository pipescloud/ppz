#!/usr/bin/env bash
# Multi-tenant scoping: /dashboard must list only orgs the signed-in
# user owns OR is a member of — not every org in the database.
#
# Bug repro: a fresh OAuth signup for "james" landed on /dashboard and
# saw alpha + beta (seed orgs james has no relation to) alongside the
# jamesmiles org auto-created on signup. handleGUIIndex was calling
# db.ListOrganisations (unscoped). With multiple real users this is a
# tenant leak.
. /tests/lib/common.sh

# Add an isolated org that no seeded user owns or is a member of —
# strengthens the assertion that we're filtering, not just renaming.
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -v ON_ERROR_STOP=1 -tAc "
  INSERT INTO accounts (id, name, owner_user_id)
  SELECT '99999999-9999-9999-9999-999999999999', 'tenant-leak-canary', u.id
    FROM users u WHERE u.username = 'unauthenticated'
  ON CONFLICT (id) DO NOTHING
" >/dev/null

dashboard_for() {
    local user="$1"
    local jar; jar=$(mktemp)
    curl_server "/dev/login?user=$user" -X POST -c "$jar" -o /dev/null -s
    curl_server "/dashboard" -b "$jar" -s
    rm -f "$jar"
}

# Seed memberships (from internal/seed/seed.go + tests/lib/reset.sh):
#   foo: owns alpha, member of alpha
#   bar: member of alpha + beta
#   neither is a member of tenant-leak-canary

echo "--- foo (owns+member alpha; nothing else) ---"
body=$(dashboard_for foo)
echo "alpha_present:               $(echo "$body" | matches '>alpha<' && echo true || echo false)"
echo "beta_present:                $(echo "$body" | matches '>beta<' && echo true || echo false)"
echo "tenant_leak_canary_present:  $(echo "$body" | matches 'tenant-leak-canary' && echo true || echo false)"

echo ""
echo "--- bar (member of alpha + beta) ---"
body=$(dashboard_for bar)
echo "alpha_present:               $(echo "$body" | matches '>alpha<' && echo true || echo false)"
echo "beta_present:                $(echo "$body" | matches '>beta<' && echo true || echo false)"
echo "tenant_leak_canary_present:  $(echo "$body" | matches 'tenant-leak-canary' && echo true || echo false)"
