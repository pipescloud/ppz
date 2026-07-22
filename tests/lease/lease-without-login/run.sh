#!/usr/bin/env bash
# RED: `ppz terminal lease` requires an authenticated daemon; unauthenticated
# it fails with E_NOT_LOGGED_IN (exit 10), mirroring `terminal share`.
. /tests/lib/common.sh
ppz_a terminal lease box 60s
