#!/usr/bin/env bash
# RED: `ppz source clear` must surface as an unknown top-level command.
# Replacement: ppz unset handle (locked decision #20).
. /tests/lib/common.sh

stderr=$(ppz_a source clear 2>&1 1>/dev/null)
rc=$?

echo "exit=$rc"
if echo "$stderr" | matches -E 'unknown (command|subcommand)'; then
  echo "stderr_contains_unknown=yes"
else
  echo "stderr_contains_unknown=no"
fi
