#!/usr/bin/env bash
# RED: `ppz terminal control` requires login; unauthenticated -> exit 10,
# checked before any attach so it never blocks reading stdin.
. /tests/lib/common.sh
ppz_a terminal control box </dev/null
