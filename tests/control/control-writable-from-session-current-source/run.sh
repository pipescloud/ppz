#!/usr/bin/env bash
# RED (regression, reproduces a production report): `ppz terminal control` run
# from an ordinary shell — where PPZ_CURRENT_HANDLE is UNSET and the controller
# identity comes from the session's CURRENT SOURCE (exactly how `ppz send` /
# `ppz command` already resolve the sender) — must attach WRITABLE and forward
# keystrokes.
#
# The bug: control derived its identity from os.Getenv("PPZ_CURRENT_HANDLE")
# (empty here), but the daemon stamps the lease-acquire's envelope sender from
# the session's current source. So the host granted the lease to the current
# source ("operator"), control compared holder("operator") against ""(env),
# saw a mismatch, and downgraded to READ-ONLY — locking the operator out of the
# lease it just won. Every prior control/lease test set PPZ_CURRENT_HANDLE
# explicitly, masking this path.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

# PTY host for 'box' (new code → runs the lease manager that grants).
PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share box -- \
  sh -c 'stty -echo 2>/dev/null; exec cat' >/dev/null 2>&1 &
TERM_PID=$!
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^box.system'" >/dev/null

# Controller identity comes from the session's current source — NOT from
# PPZ_CURRENT_HANDLE. This is the ordinary-shell case.
ppz_a source create operator >/dev/null

# Drive control WITHOUT PPZ_CURRENT_HANDLE set; hold stdin open while the byte
# round-trips.
( printf 'typed-when-writable\n'; sleep 3 ) \
  | PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal control box >/tmp/ctlC.out 2>&1 &
CTL_PID=$!

wait_for 30 "ppz_a reread box.stdout 2>/dev/null | grep -q typed-when-writable" >/dev/null

echo "keystroke_delivered: $(ppz_a reread box.stdout | grep -c typed-when-writable)"
echo "attached_readonly: $(grep -c 'attaching read-only' /tmp/ctlC.out)"

kill "$CTL_PID" 2>/dev/null || true
kill "$TERM_PID" 2>/dev/null || true
wait "$CTL_PID" 2>/dev/null || true
wait "$TERM_PID" 2>/dev/null || true
