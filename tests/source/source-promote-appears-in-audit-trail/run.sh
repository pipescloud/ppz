#!/usr/bin/env bash
# Promoting a source to a terminal (bare `ppz terminal share`, which hits
# POST /api/v1/sources/{handle}/ensure-pty) changes what the source IS:
# it provisions the reserved system/stdin/stdout pipe set, so afterwards
# the handle can be driven, not just read. That belongs in the trail.
#
# Idempotency is the load-bearing half of this scenario. Bare `ppz
# terminal share` calls ensure-pty on EVERY invocation, so auditing the
# request rather than the state change would bury the org's real history
# under a source.promote row per share. Exactly one row must exist after
# two calls.
#
# RED until source.promote is recorded, and recorded only on the flip.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

ensure_pty() {
  curl -sS -X POST "$PPZ_SERVER_URL/api/v1/sources/chat/ensure-pty" \
    -H "Authorization: Bearer $(key_alpha)" \
    -H "Content-Type: application/json" -d '{}' -o /dev/null
}
ensure_pty
ensure_pty

echo "--- one row for the flip, not one per call ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT count(*)::text FROM audit_events WHERE action = 'source.promote'
"

echo "--- the row states the kind change ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT target, before::text, after::text FROM audit_events
   WHERE action = 'source.promote'
" | sed 's/|/ /g'
