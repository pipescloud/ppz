#!/usr/bin/env bash
# ACL Phase 2: the roster is visible to any principal holding ANY
# access on the pipe, and to nobody else.
#
# bar holds write on alice.inbox by default (write-only — no read), so
# bar must still see the roster: an inbox sender needs to know who to
# ask. bar holds nothing on alice.stdout, so that roster is refused.
#
# The write-only case is the one most likely to be got wrong — a naive
# "can you read it" gate would hide the roster from every sender.
#
# RED until Phase 2 lands.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create alice >/dev/null
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null

echo "--- bar holds write-only on alice.inbox, so sees its roster ---"
ppz_b pipe acl ls alice.inbox --json | jq -r '.[].principal' | sort

echo "--- bar holds nothing on alice.stdout, so is refused ---"
ppz_b pipe acl ls alice.stdout 2>&1 | tail -1
