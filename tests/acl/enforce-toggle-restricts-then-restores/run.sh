#!/usr/bin/env bash
# ACL Phase 3: toggling enforcement takes effect without a restart, and
# disabling is non-destructive.
#
# Toggling bumps acl_generation and nudges <A>._system.acl, so live
# daemons re-exchange and pick up (or drop) compiled credentials on the
# spot — the same invalidation path a revoke uses. Grant rows survive a
# disable, so toggling back restores the prior configuration exactly.
#
# RED until Phase 3 lands.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null
ppz_a source create alice >/dev/null
ppz_a pipe create alice.notes >/dev/null
ppz_a send alice.notes "secret" >/dev/null

echo "--- off: bar can read ---"
ppz_b read alice.notes --bare

echo "--- enable ---"
ppz_a acl enforce on >/dev/null
ppz_a acl enforce --json | jq -r '.enforced'

echo "--- on: bar is denied, without restarting anything ---"
if ppz_b reread alice.notes --bare >/dev/null 2>&1; then echo "UNEXPECTED: read served"; else echo "denied"; fi

echo "--- grant bar read, then it works ---"
ppz_a pipe acl grant alice.notes bar read >/dev/null
ppz_b reread alice.notes --bare

echo "--- disable: grants persist ---"
ppz_a acl enforce off >/dev/null
ppz_a pipe acl ls alice.notes --json | jq -r '[.[] | select(.principal=="bar")] | length'
