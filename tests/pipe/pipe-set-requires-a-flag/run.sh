#!/usr/bin/env bash
# `ppz pipe set NAME` with no retention flag names no change. It fails
# locally (exit 20, E_INVALID_PIPE) rather than round-tripping to the
# server to be told nothing happened.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null
ppz_a pipe create chat.archive >/dev/null

ppz_a pipe set chat.archive
