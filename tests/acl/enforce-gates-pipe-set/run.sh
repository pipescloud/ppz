#!/usr/bin/env bash
# ACL Phase 3: `ppz pipe set` is an admin operation.
#
# Changing a pipe's retention is exactly what `admin` is defined to
# cover, and pipe set deliberately reaches reserved auto-pipes that
# `pipe create` can never name — so an ungated pipe set would let any
# member shorten the TTL on someone else's stdout and silently discard
# their history.
#
# Gated only while enforcement is on: with it off, nothing about the
# org's behaviour changes.
#
# RED until pipe set consults the ACL.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null
ppz_a source create ops >/dev/null
ppz_a pipe create ops.deploy-log >/dev/null

echo "--- enforcement off: bar may still set retention ---"
if ppz_b pipe set ops.deploy-log --ttl=1h >/dev/null 2>&1; then echo "allowed"; else echo "UNEXPECTED: denied while off"; fi

ppz_a acl enforce on >/dev/null

echo "--- enforcement on: bar holds no admin on ops.deploy-log ---"
if ppz_b pipe set ops.deploy-log --ttl=2h >/dev/null 2>&1; then
  echo "UNEXPECTED: member changed retention on a pipe it cannot administer"
else
  echo "denied"
fi

echo "--- the pipe's owner still can ---"
ppz_a pipe set ops.deploy-log --ttl=3h >/dev/null 2>&1 && echo "owner allowed" || echo "UNEXPECTED: owner denied"
