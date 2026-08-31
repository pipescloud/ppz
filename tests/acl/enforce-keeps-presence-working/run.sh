#!/usr/bin/env bash
# Presence must keep working once enforcement is on.
#
# `heartbeat` does not ride its logical path — Phase 0b routes it to
# <account>._presence.<handle>. If the credential compiler emits pub
# entries from the logical path instead of the wire subject, every
# heartbeat publish in the org is denied the moment an admin flips
# enforcement on.
#
# It fails SILENTLY: the heartbeat ticker discards its publish error
# (`_ = deps.Publish(...)`), so nothing surfaces except `ppz who` quietly
# going empty — for everyone, including the org owner, who also gets a
# compiled credential under enforcement.
#
# Two bugs meet here. The compiler must emit the WIRE subject rather than
# the logical path, and creating a source under enforcement must
# invalidate credentials — otherwise the principal cannot publish to the
# pipes it just created until its credential happens to expire.
. /tests/lib/common.sh

cleanup() { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
trap cleanup EXIT

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

echo "--- enable enforcement first ---"
ppz_a acl enforce on >/dev/null
ppz_a acl enforce --json | jq -r '.enforced'

echo "--- an agent started under enforcement still reports presence ---"
ppz_a terminal share alice -- sleep 30 </dev/null >/dev/null 2>&1 &
PID=$!
if wait_for 100 "ppz_a who | grep -q '^alice'"; then
  echo "presence=live"
else
  echo "presence=MISSING — heartbeat publishes are being denied"
fi
