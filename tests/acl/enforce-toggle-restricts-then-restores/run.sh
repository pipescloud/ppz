#!/usr/bin/env bash
# ACL Phase 3: toggling enforcement takes effect without a restart, and
# disabling is non-destructive.
#
# NATS evaluates permissions only at connect, so a toggle does not reach
# a live connection on its own. The server publishes on
# <account>._system.acl and every daemon re-exchanges and redials — that
# path is what this scenario exercises. Propagation is asynchronous
# (publish -> refresh -> HTTP -> reconnect), so the assertions are
# bounded waits rather than immediate checks; the point being proven is
# that it lands at all without anyone restarting a daemon.
#
# The budget is generous on purpose. The e2e stack runs a deliberately
# short PPZ_NATS_JWT_TTL to exercise rotation, so the daemon is already
# refreshing constantly and a forced re-exchange contends with the
# periodic one. The contract being asserted is "converges without anyone
# restarting a daemon", not "instantly".
#
# Grant rows survive a disable, so toggling back on restores the prior
# configuration exactly.
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

echo "--- grant bar read, then it works ---"
ppz_a pipe acl grant alice.notes bar read >/dev/null
wait_for 200 "ppz_b reread alice.notes >/dev/null 2>&1" || echo "UNEXPECTED: grant never took effect"
ppz_b reread alice.notes --bare

echo "--- disable: grants persist ---"
ppz_a acl enforce off >/dev/null
ppz_a pipe acl ls alice.notes --json | jq -r '[.[] | select(.principal=="bar")] | length'
