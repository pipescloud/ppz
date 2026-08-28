#!/usr/bin/env bash
# ACL Phase 3: enabling enforcement restricts a daemon that is already
# running and already connected.
#
# NATS evaluates permissions only at connect, so the switch does not
# reach a live connection by itself. The server publishes on
# <account>._system.acl; the daemon re-exchanges and redials. That path
# is what this asserts — nobody restarts anything.
#
# Bounded wait rather than an immediate check: the contract is
# "converges", not "instantly". Split from the grant/disable half so
# each stays well inside the harness's 30s per-scenario cap.
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
if wait_for 200 "! ppz_b reread alice.notes >/dev/null 2>&1"; then
  echo "denied"
else
  echo "UNEXPECTED: read still served after enforcement was enabled"
fi
