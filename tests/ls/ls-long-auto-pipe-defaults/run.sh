#!/usr/bin/env bash
# Auto-provisioned pipes have no `pipes` row until someone runs
# `pipe set` on them, so a DB-sourced answer would be blank for exactly
# the pipes whose caps users hit first.
#
# `ls -l` reads the caps from the JetStream stream config instead — the
# thing actually doing the enforcing — so an untouched `inbox` reports
# the real built-in defaults rather than nothing.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

echo "--- untouched auto-pipe reports the built-in defaults ---"
ppz_a ls -l | ls_normalize | awk '$1 == "chat.inbox" {print "ttl="$4" msgs="$5" bytes="$6}'

echo "--- and tracks a set ---"
ppz_a pipe set chat.inbox --max-msgs=3 >/dev/null
ppz_a ls -l | ls_normalize | awk '$1 == "chat.inbox" {print "ttl="$4" msgs="$5" bytes="$6}'
