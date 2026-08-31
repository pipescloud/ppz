#!/usr/bin/env bash
# ACL Phase 3: the preview tells an admin what enabling would break.
#
# A pty source's stdio becomes owner-only on enable — the largest
# visible change and the one an org is most likely to be relying on. The
# preview is computed from the derived defaults, not from observed
# traffic, so it reports the shared terminal whether or not anyone has
# watched it recently.
#
# RED until Phase 3 lands.
. /tests/lib/common.sh

cleanup() { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
trap cleanup EXIT

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a terminal share alice -- sleep 10 </dev/null &
PID=$!
wait_for 50 "ppz_a ls | grep -q alice" || { echo "timeout: alice never appeared"; exit 1; }

echo "--- shared terminals that would become private ---"
ppz_a acl preview --json | jq -r '.shared_terminals[] | "\(.handle) owner=\(.owner)"'

echo "--- uncollared pipes are unaffected and not listed ---"
ppz_a pipe create room >/dev/null
ppz_a acl preview --json | jq -r '[.shared_terminals[].handle] | index("room") // "absent"'
