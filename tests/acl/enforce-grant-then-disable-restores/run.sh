#!/usr/bin/env bash
# ACL Phase 3: under enforcement a grant opens access back up, and
# disabling enforcement is non-destructive.
#
# Grant rows survive a disable, so switching back on restores exactly
# the previous configuration rather than resetting to defaults.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null
ppz_a source create alice >/dev/null
ppz_a pipe create alice.notes >/dev/null
ppz_a send alice.notes "secret" >/dev/null
ppz_a acl enforce on >/dev/null

echo "--- grant bar read, then it works ---"
ppz_a pipe acl grant alice.notes bar read >/dev/null
if wait_for 200 "ppz_b reread alice.notes >/dev/null 2>&1"; then
  ppz_b reread alice.notes --bare
else
  echo "UNEXPECTED: grant never took effect"
fi

echo "--- disable: grants persist ---"
ppz_a acl enforce off >/dev/null
ppz_a pipe acl ls alice.notes --json | jq -r '[.[] | select(.principal=="bar")] | length'
