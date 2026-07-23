#!/usr/bin/env bash
# RED: `ppz org list` must surface as an unknown top-level command.
#
# `ppz org` is being removed pre-launch — multi-tenancy moves to the
# pipescloud control plane; OSS keeps single-tenant accounts as the
# default deployment shape. See the OSS-PIPESCLOUD-ARCHITECTURE-SPLIT
# strategy doc (private), locked decisions #11 and #18.
#
# This test fails today (the verb still dispatches to cmdOrg) and
# passes after commit 3 of the Phase 1 PR removes the verb.
. /tests/lib/common.sh

stderr=$(ppz_a org list 2>&1 1>/dev/null)
rc=$?

echo "exit=$rc"
if echo "$stderr" | matches -E 'unknown (command|subcommand)'; then
  echo "stderr_contains_unknown=yes"
else
  echo "stderr_contains_unknown=no"
fi
