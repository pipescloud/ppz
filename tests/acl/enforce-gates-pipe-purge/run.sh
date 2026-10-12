#!/usr/bin/env bash
# Purging discards history, so like destroy and `pipe set` it needs admin
# on the pipe while the org enforces ACLs — and nothing extra while off.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null
ppz_a source create ops >/dev/null
ppz_a pipe create ops.deploy-log >/dev/null

echo "--- enforcement off: bar may purge ---"
ppz_b pipe purge ops.deploy-log >/dev/null 2>&1 && echo "allowed" || echo "UNEXPECTED: denied while off"

ppz_a acl enforce on >/dev/null
echo "--- enforcement on: bar holds no admin on ops.deploy-log ---"
ppz_b pipe purge ops.deploy-log >/dev/null 2>&1 && echo "UNEXPECTED: allowed" || echo "denied"

echo "--- the pipe's owner still can ---"
ppz_a pipe purge ops.deploy-log >/dev/null 2>&1 && echo "owner allowed" || echo "UNEXPECTED: owner denied"
