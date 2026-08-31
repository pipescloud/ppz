#!/usr/bin/env bash
# ACL Phase 2: access that exists with no stored row still explains
# itself.
#
# Nobody has granted anything on alice.inbox, yet every member can
# write to it. `ppz acl whoami` must say so and say why — this is the
# case a grant-table view cannot show at all.
#
# RED until Phase 2 lands.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create alice >/dev/null

# key-alpha2's principal is bar, a member of alpha but not alice's owner.
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null

echo "--- bar on alice.inbox ---"
ppz_b acl whoami alice.inbox --json | jq -c '{read, write, admin}'

echo "--- why ---"
ppz_b acl whoami alice.inbox --json | jq -r '.why.write'
