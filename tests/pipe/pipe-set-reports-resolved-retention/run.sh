#!/usr/bin/env bash
# The printed line states the pipe's COMPLETE retention after the change,
# not just the field that moved — the same shape `pipe create` prints, so
# both commands answer "what does this pipe retain now?" identically.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null
ppz_a pipe create chat.archive >/dev/null

echo "--- ttl only ---"
ppz_a pipe set chat.archive --ttl=168h

echo "--- max-bytes only (suffixed size) ---"
ppz_a pipe set chat.archive --max-bytes=64MiB

echo "--- all three at once ---"
ppz_a pipe set chat.archive --ttl=1h --max-msgs=10 --max-bytes=1MB
