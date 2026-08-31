#!/usr/bin/env bash
# ACL Phase 3: enforcement is off for every org until an admin opts in.
#
# THE upgrade-safety property. Every existing deployment ships with
# acl_enforced=false, so every existing flow must keep working with no
# grants in place and nobody having touched the Security tab.
#
# bar holds nothing on alice.stdout by the derived defaults, and must
# still be able to read it while enforcement is off.
#
# RED until Phase 3 lands.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null
ppz_a source create alice >/dev/null
ppz_a pipe create alice.notes >/dev/null
ppz_a send alice.notes "hello" >/dev/null

echo "--- enforcement state ---"
ppz_a acl enforce --json | jq -r '.enforced'

echo "--- bar holds nothing by the defaults ---"
ppz_b acl whoami alice.notes --json | jq -c '{read, write, enforced}'

echo "--- but can still read it, because nothing is enforced ---"
ppz_b read alice.notes --bare
