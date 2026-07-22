#!/usr/bin/env bash
# Phase 2 regression: the existing API-key path
# `ppz daemon login URL -apikey K` continues to work after we rewire
# requireBearer. Old daemons + CI workflows must not break.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

echo "--- daemon-a status reports logged-in ---"
ppz_a status | matches -E 'logged in' && echo "login=ok" || echo "login=fail"

echo ""
echo "--- ls works against the API ---"
ppz_a ls >/dev/null && echo "ls=ok" || echo "ls=fail"
