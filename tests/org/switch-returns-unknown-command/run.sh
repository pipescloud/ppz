#!/usr/bin/env bash
# RED: `ppz org switch <name>` must surface as an unknown top-level
# command after Phase 1 strips the org verb. See sibling test
# tests/org/list-returns-unknown-command/run.sh for context.
. /tests/lib/common.sh

stderr=$(ppz_a org switch beta 2>&1 1>/dev/null)
rc=$?

echo "exit=$rc"
if echo "$stderr" | matches -E 'unknown (command|subcommand)'; then
  echo "stderr_contains_unknown=yes"
else
  echo "stderr_contains_unknown=no"
fi
