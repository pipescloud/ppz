#!/usr/bin/env bash
# RED: bare `ppz terminal share` on an inbox-only (message-kind) source must
# UPGRADE it to a full terminal — provisioning the COMPLETE pty pipe set
# (heartbeat, inbox, stdctrl, stdin, stdout, system), not just the IO subset.
# The `system` pipe is the tell: it's a reserved name the user pipe-create
# path can't add, so it's absent until bare-share upgrades the source via the
# trusted provisioning path. Without system there is no write-lease pipe, so
# the "terminal" can be watched but never controlled.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

# Bare share (no handle → current source 'chat', a message source), long-lived
# child so the wrapper stays up while we enumerate its pipes.
ppz_a terminal share -- sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^chat.stdout'" >/dev/null
# Give heartbeat (published on an interval) a moment to register its pipe too.
wait_for 30 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^chat.heartbeat'" >/dev/null

# The full pty pipe set, sorted. Any missing entry (currently: chat.system)
# fails the diff.
ppz_a ls | ls_normalize | grep -oE '^chat\.[a-z]+' | sort -u

kill "$TERM_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
