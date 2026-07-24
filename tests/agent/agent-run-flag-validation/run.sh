#!/usr/bin/env bash
# Surface-level validation for `ppz agent run` (the cut-down sibling of
# `agent create`). Covers the cases that need neither a real harness
# binary nor an interactive tty:
#
#   1. missing handle — the usage error must name the `run` verb (not
#      `create`), so the message echoes what the user typed.
#   2. shared harness-flag validation — run reuses create's parser, so
#      the mutual-exclusion error is identical apart from the verb.
#   3. --new-window rejection — run is foreground-only in this cut.
#   4. the non-tty guard — the test runner's stdin is never a tty, so
#      `agent run <handle>` with otherwise-valid flags fails preflight
#      before any daemon call.
#
# All four abort before the daemon is consulted, so this scenario is a
# pure CLI-surface check.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

echo "--- 1: missing handle (names the run verb) ---"
ppz_a agent run 2>&1; echo "rc=$?"

echo "--- 2: --claude --codex (shared harness validation) ---"
ppz_a agent run --claude --codex test-x 2>&1; echo "rc=$?"

echo "--- 3: --new-window rejected (run is foreground-only) ---"
ppz_a agent run test-x --new-window 2>&1; echo "rc=$?"

echo "--- 4: non-tty stdin rejected ---"
ppz_a agent run test-x 2>&1; echo "rc=$?"
