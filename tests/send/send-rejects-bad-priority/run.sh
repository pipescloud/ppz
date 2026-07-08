#!/usr/bin/env bash
# `ppz send --priority urgent ...` is rejected with E_INVALID_PRIORITY.
# Only 1|high, 2|medium|med, 3|low are valid (0 means "flag omitted" and
# cannot be passed explicitly). CLI rejects belt; daemon (handlers.go
# handleSend) rejects suspenders — this scenario exercises the CLI path.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

# Capture stderr + stdout — error line lands on stderr.
ppz_a send chat "hi" --priority urgent 2>&1 | grep -oE '^error: E_[A-Z_]+' || true
ppz_a send chat "hi" --priority 0 2>&1 | grep -oE '^error: E_[A-Z_]+' || true
