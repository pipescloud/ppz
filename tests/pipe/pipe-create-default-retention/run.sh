#!/usr/bin/env bash
# `ppz pipe create <name>` (no flags) provisions a JetStream stream on the
# current source with the built-in defaults: 24 h max age, 5000 max msgs,
# 16 MiB max bytes (see internal/server/streams.go). The new pipe is
# visible in `ppz ls`.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null
ppz_a pipe create chat.archive
ppz_a ls | ls_normalize | grep '^chat\.'
