#!/usr/bin/env bash
# RED: `ppz broadcast` must surface as an unknown top-level command.
#
# `ppz broadcast` is being removed pre-launch in favour of explicit
# room pipes (`ppz pipe create team1.room` for shared rooms, or a
# custom pipe with `--writers=owner` for the rare announce case).
# See OSS-PIPESCLOUD-ARCHITECTURE-SPLIT (private) locked decision #16.
. /tests/lib/common.sh

stderr=$(ppz_a broadcast some.handle hello 2>&1 1>/dev/null)
rc=$?

echo "exit=$rc"
if echo "$stderr" | matches -E 'unknown (command|subcommand)'; then
  echo "stderr_contains_unknown=yes"
else
  echo "stderr_contains_unknown=no"
fi
