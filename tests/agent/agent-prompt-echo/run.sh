#!/usr/bin/env bash
# Surface check for `ppz agent prompt`: it echoes the boot prompt the
# other agent verbs would use, without launching anything. We assert on
# the non-brittle cases — a verbatim positional prompt and the
# missing-handle usage error — rather than the large per-harness default
# (which is pinned byte-for-byte by unit tests and would make this
# expected.txt churn on every prompt-wording tweak).
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

echo "--- 1: positional prompt echoed verbatim (no launch) ---"
ppz_a agent prompt ash "just do the thing" 2>&1; echo "rc=$?"

echo "--- 2: missing handle (names the prompt verb) ---"
ppz_a agent prompt 2>&1; echo "rc=$?"
