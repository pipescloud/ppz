#!/usr/bin/env bash
# RED: when another sender already holds the lease, `ppz terminal control`
# falls back to READ-ONLY -- it still streams .stdout (operator can watch) but
# its keystrokes are NOT delivered to the child (dropped by the host, since
# control's sender != lease-holder). We prove stdout visibility via a banner
# the child prints, and prove the keystroke was dropped by pushing a
# holder-sent marker AFTER it through the same .stdin FIFO and asserting the
# control keystroke never surfaced. Not implemented yet.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'echo READY-BANNER; stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.stdin'" >/dev/null

# owner holds the lease.
PPZ_CURRENT_HANDLE=owner ppz_a terminal lease box 60s >/dev/null 2>&1

# viewer attaches with control (read-only fallback), typing a line.
( printf 'viewer-keystroke\n'; sleep 3 ) \
  | PPZ_CURRENT_HANDLE=viewer PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" \
    ppz terminal control box >/tmp/ctlB.out 2>&1 &
CTL_PID=$!

# control still sees the stream: wait for the banner in its own stdout.
wait_for 30 "grep -q READY-BANNER /tmp/ctlB.out" >/dev/null

# Push an owner marker through .stdin AFTER the (dropped) keystroke; when it
# surfaces we know the pipe has drained past viewer-keystroke.
PPZ_CURRENT_HANDLE=owner ppz_a send box.stdin $'owner-marker\n' >/dev/null
wait_for 30 "ppz_a reread box.stdout 2>/dev/null | grep -q owner-marker" >/dev/null

echo "stdout_visible: $(grep -c READY-BANNER /tmp/ctlB.out)"
echo "keystroke_delivered: $(ppz_a reread box.stdout | grep -c viewer-keystroke)"

kill "$CTL_PID" 2>/dev/null || true
kill "$TERM_PID" 2>/dev/null || true
wait "$CTL_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
