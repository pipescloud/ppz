#!/usr/bin/env bash
# Successive `pipe set` calls MERGE onto stored retention rather than
# clobbering it back to defaults. Setting --max-msgs then --ttl must
# leave the earlier max-msgs in place — the failure mode this pins is a
# server that writes the whole triple from a partial request.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null
ppz_a pipe create chat.archive >/dev/null

ppz_a pipe set chat.archive --max-msgs=7 >/dev/null
# Second call names only --ttl; msgs=7 must survive it.
ppz_a pipe set chat.archive --ttl=1h
# Third names only --max-bytes; both earlier values must survive.
ppz_a pipe set chat.archive --max-bytes=1MiB
