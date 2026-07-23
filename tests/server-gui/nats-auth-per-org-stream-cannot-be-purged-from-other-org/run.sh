#!/usr/bin/env bash
# Phase 3.5 control-plane isolation: a daemon authenticated into
# org alpha cannot destroy org beta's data, even when it knows beta's
# stream name and issues a JS API call directly.
#
# Pre-3.5 (subject-prefix isolation): all daemons shared the
# `ppz-tenants` account; STREAM.PURGE was a real cross-tenant
# threat (auth allowed it; only the missing target spared the data).
#
# Post-3.5 (per-org accounts): alpha's user JWT is bound to the
# tenant-alpha NATS account. JS API calls from alpha route to
# alpha's JS instance — beta's streams aren't visible there at all.
# The call returns "stream not found" because, from alpha's
# perspective, the stream truly doesn't exist in alpha's namespace.
#
# This test asserts the load-bearing security property: beta's data
# is intact after alpha tries to purge it. The mechanism (auth
# denial vs cross-account invisibility) is incidental.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_beta)" >/dev/null

ppz_b source create canary >/dev/null
ppz_b send canary.inbox "beta-data" >/dev/null
wait_for 20 "ppz_b ls | ls_normalize | grep -q '^canary.inbox'" >/dev/null
BETA_ORG=$(jq -r '.org_id' "${PPZ_DAEMON_B_HOME:-/tmp/b}/credentials")
BETA_STREAM="source_${BETA_ORG}_canary_inbox"
echo "beta_stream_set_up: $([[ -n "$BETA_STREAM" ]] && echo true || echo false)"

JWT=$(jq -r '.nats_user_jwt' "${PPZ_DAEMON_A_HOME:-/tmp/a}/credentials")
SEED=$(jq -r '.nats_user_seed' "${PPZ_DAEMON_A_HOME:-/tmp/a}/credentials")
CREDS=$(mktemp)
trap "rm -f $CREDS" EXIT
cat > "$CREDS" <<EOF
-----BEGIN NATS USER JWT-----
$JWT
------END NATS USER JWT------

-----BEGIN USER NKEY SEED-----
$SEED
------END USER NKEY SEED------
EOF

echo ""
echo "--- alpha JWT issues STREAM.PURGE on beta's stream ---"
nats --server="nats://ppz-server:4222" --creds="$CREDS" \
    request "\$JS.API.STREAM.PURGE.${BETA_STREAM}" '{}' --timeout=3s >/dev/null 2>&1 || true

echo ""
echo "--- beta's data is still intact ---"
# `matches`, not `grep -q`: a short-circuiting grep would SIGPIPE the
# upstream ls_normalize (awk/sed) and flip this security assertion to a
# false failure. See matches in common.sh.
if ppz_b ls | ls_normalize | matches -E 'canary.inbox +1 +1 .*beta-data'; then
  echo "beta_data_still_intact: true"
else
  echo "beta_data_still_intact: false"
fi
