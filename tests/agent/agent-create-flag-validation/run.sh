#!/usr/bin/env bash
# Surface-level validation for `ppz agent create` (v0.26 verb).
#
# Covers the cases that don't need a real harness binary or osascript:
# missing handle, mutually-exclusive harness flags, mutually-exclusive
# claude shortcuts, claude shortcut on non-claude harness, positional
# prompt + --prompt-file conflict, and — the one that motivated this
# scenario — that --new-window is accepted by the flag parser.
#
# In RED the binary aborts at flag.Parse with "flag provided but not
# defined: -new-window" because --new-window was missing from the
# FlagSet; in GREEN it gets past parse and fails downstream when the
# Linux test container can't exec `osascript`. We only assert on the
# parse outcome — that's the bug this test pins.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

echo "--- 1: missing handle ---"
ppz_a agent create 2>&1; echo "rc=$?"

echo "--- 2: --claude --codex (mutually exclusive harnesses) ---"
ppz_a agent create --claude --codex test-x 2>&1; echo "rc=$?"

echo "--- 3: --opus --sonnet (mutually exclusive shortcuts) ---"
ppz_a agent create --opus --sonnet test-x 2>&1; echo "rc=$?"

echo "--- 4: --opus on --codex (claude-only shortcut) ---"
ppz_a agent create --codex --opus test-x 2>&1; echo "rc=$?"

echo "--- 5: positional + --prompt-file conflict ---"
ppz_a agent create test-x positional --prompt-file /etc/hostname 2>&1; echo "rc=$?"

echo "--- 6: --new-window flag is accepted by parser ---"
out=$(ppz_a agent create test-y --new-window 2>&1 || true)
if echo "$out" | matches "flag provided but not defined"; then
  echo "FAIL: --new-window rejected by flag parser"
else
  echo "OK: --new-window accepted by flag parser"
fi
