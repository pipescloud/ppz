#!/usr/bin/env bash
# RED: `ppz source switch` must surface as an unknown top-level command.
# Replacement: ppz set handle HANDLE (locked decision #20).
. /tests/lib/common.sh

stderr=$(ppz_a source switch cindy 2>&1 1>/dev/null)
rc=$?

echo "exit=$rc"
if echo "$stderr" | matches -E 'unknown (command|subcommand)'; then
  echo "stderr_contains_unknown=yes"
else
  echo "stderr_contains_unknown=no"
fi
