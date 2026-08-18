#!/usr/bin/env bash
# ACL Phase 2: grant, then see the row — with its provenance.
#
# alice.stdout is owner-only by default. foo grants bar read, and the
# roster must show BOTH the derived default (alice, as handle owner)
# and the stored grant (bar, by whom and when). A view that rendered
# only acl_grants would show one row and imply nobody else can reach
# it.
#
# RED until Phase 2 lands.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a terminal share alice -- sleep 5 </dev/null &
PID=$!
trap 'kill $PID 2>/dev/null; wait $PID 2>/dev/null' EXIT
wait_for 50 "ppz_a ls | grep -q alice" || { echo "timeout: alice never appeared"; exit 1; }

echo "--- before the grant ---"
ppz_a pipe acl ls alice.stdout --json | jq -c '[.[] | {principal, read, via}] | sort_by(.principal)'

echo "--- grant bar read ---"
ppz_a pipe acl grant alice.stdout bar read

echo "--- after the grant ---"
ppz_a pipe acl ls alice.stdout --json | jq -c '[.[] | {principal, read, via}] | sort_by(.principal)'

echo "--- revoke is idempotent ---"
ppz_a pipe acl revoke alice.stdout bar read
ppz_a pipe acl revoke alice.stdout bar read
