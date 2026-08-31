#!/usr/bin/env bash
# ACL Phase 3: an unenforced answer must say so.
#
# A surface reporting "you cannot read this" while the read demonstrably
# succeeds is worse than no surface — it is a security control that
# lies, and it lies most convincingly to whoever is checking whether
# they are safe.
#
# RED until Phase 3 lands.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null
ppz_a source create alice >/dev/null

echo "--- text output carries the caveat ---"
ppz_b acl whoami alice.stdout | grep -ci "not enforced"

echo "--- and so does the roster ---"
ppz_b acl whoami alice.inbox | grep -ci "not enforced"
