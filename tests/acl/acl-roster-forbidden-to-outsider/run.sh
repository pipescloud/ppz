#!/usr/bin/env bash
# ACL Phase 2: the roster is visible to any principal holding ANY access
# on the pipe, and to nobody else.
#
# bar holds write on alice.inbox by default (write-only — no read), so
# bar must still see the roster: an inbox sender needs to know who to
# ask. bar holds nothing on alice.stdout, so that roster is refused.
#
# The write-only case is the one most easily got wrong — a naive "can you
# read it" gate would hide the roster from every sender in the org.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create alice >/dev/null
# key-alpha2's principal is bar: a member of alpha, not its owner.
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null

echo "--- bar holds write-only on alice.inbox ---"
ppz_b acl whoami alice.inbox --json | jq -c '{read, write}'

echo "--- and so can see its roster ---"
ppz_b pipe acl ls alice.inbox --json | jq -r 'sort_by(.principal) | .[].principal'

echo "--- bar holds nothing on alice.stdout, so is refused ---"
if ppz_b pipe acl ls alice.stdout >/dev/null 2>&1; then
  echo "UNEXPECTED: roster was served"
else
  echo "refused"
fi
