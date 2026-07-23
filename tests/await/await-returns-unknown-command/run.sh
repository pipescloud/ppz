#!/usr/bin/env bash
# RED: `ppz await` must surface as an unknown top-level command.
#
# `ppz await` was removed — it was the wrong abstraction for agents.
# Its drain-on-arrival semantics raced any later `ppz read inbox`
# (the agent claimed it acted while the user's read showed nothing).
# Awareness is now `ppz ls --watch` (non-destructive wake signal);
# consumption is `ppz read`.
. /tests/lib/common.sh

stderr=$(ppz_a await some.pipe 2>&1 1>/dev/null)
rc=$?

echo "exit=$rc"
if echo "$stderr" | matches -E 'unknown (command|subcommand)'; then
  echo "stderr_contains_unknown=yes"
else
  echo "stderr_contains_unknown=no"
fi
