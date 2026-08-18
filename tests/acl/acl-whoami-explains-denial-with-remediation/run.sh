#!/usr/bin/env bash
# ACL Phase 2: a denial names the fix and who can apply it.
#
# An agent that gets denied must be able to ask the right principal
# rather than fail opaquely — so whoami prints the exact grant command
# and the principals able to run it.
#
# RED until Phase 2 lands.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create alice >/dev/null
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null

echo "--- bar is denied read on alice.stdout ---"
ppz_b acl whoami alice.stdout --json | jq -r '.read'

echo "--- and is told why ---"
ppz_b acl whoami alice.stdout --json | jq -r '.why.read'

echo "--- and how to fix it ---"
ppz_b acl whoami alice.stdout --json | jq -r '.remediation.command'

echo "--- and who can run it ---"
ppz_b acl whoami alice.stdout --json | jq -r '.remediation.runnable_by[]' | sort
