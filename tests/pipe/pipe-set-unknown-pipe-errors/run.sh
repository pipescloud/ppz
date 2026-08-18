#!/usr/bin/env bash
# `pipe set` mutates an existing pipe; it never creates one. A name that
# is neither a table row nor an auto-pipe of the source is E_PIPE_NOT_FOUND
# (exit 22), not a silent create.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

ppz_a pipe set chat.nope --max-msgs=5
