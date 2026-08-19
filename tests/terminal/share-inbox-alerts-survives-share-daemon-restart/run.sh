#!/usr/bin/env bash
# RED → GREEN: messages published to <handle>.inbox must keep reaching
# the wrapped child's terminal-alert pump after the share-side daemon
# is recycled.
#
# Bug: forwardInboxAlerts (terminal.go) opens one IPC connection at
# share-start and has no redial loop. After the daemon stops, the
# scanner returns EOF and the goroutine exits — forever. New .inbox
# messages never reach the pump, so the "Please run 'ppz subs read'"
# alert never fires for the rest of the session, even though the share
# itself recovers (forwardStdin already has a redial loop).
#
# Wrapped child is `cat`: every byte that lands on the PTY slave's
# read side (whether from stdin-relay or from the alert pump's direct
# PTY write) gets bounced to the slave's write side → master read →
# publisher → <handle>.stdout. That makes alert-pump injections
# observable via `reread <handle>.stdout`.
. /tests/lib/common.sh

ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

HOME_S=/tmp/share-inbox
rm -rf "$HOME_S"; mkdir -p "$HOME_S"
SOCK_S=$HOME_S/daemon.sock

# Dial idle + cooldown to sub-second so two alert cycles fit within
# the harness 30s ceiling.
export PPZ_TERMINAL_INBOX_IDLE_MS=200
export PPZ_TERMINAL_INBOX_COOLDOWN_MS=200
# Ceiling pinned to the base so repeat alerts keep a FLAT 200ms cadence
# here. Repeats normally back off geometrically (see CooldownMax in
# terminal_subs_alert.go), and msg-1 below is deliberately never read,
# so the ladder would otherwise climb past this scenario's wait budget
# while it is busy recycling the daemon. This fixture is about the
# forwarder redialling after a daemon swap, not about cadence — the
# ladder itself is covered by
# share-subs-alert-backs-off-when-unread-persists.
export PPZ_TERMINAL_INBOX_COOLDOWN_MAX_MS=200

ppz_s() { PPZ_HOME=$HOME_S PPZ_IPC_SOCKET=$SOCK_S ppz "$@"; }

cleanup() {
  PID=$(cat "$HOME_S/daemon.pid" 2>/dev/null || true)
  [[ -n "$PID" ]] && kill "$PID" 2>/dev/null || true
  kill "$SHARE_PID" 2>/dev/null || true
  wait "$SHARE_PID" 2>/dev/null || true
}
trap cleanup EXIT

ppz_s daemon start >/dev/null
ppz_s daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

# Wrapped command: switch the PTY slave to raw line discipline (no
# ICANON, no ECHO) so individual bytes flow through `cat` without
# waiting for a newline. The alert pump's message (submitInputForHarness
# shape: text + harness-specific submit terminator, no trailing
# newline) only round-trips to .stdout under raw mode.
ppz_s terminal share share-inbox -- bash -c 'stty -icanon -echo 2>/dev/null; cat' </dev/null >/dev/null 2>&1 &
SHARE_PID=$!

wait_for 50 "ppz_s ls 2>/dev/null | grep -q '^share-inbox.stdout'"

# Pre-recycle: one .inbox publish → after idle timer, alert fires →
# alert text round-trips through cat → lands on .stdout.
ppz_b send share-inbox.inbox "msg-1" >/dev/null
wait_for 50 "ppz_s reread share-inbox.stdout --raw 2>/dev/null | grep -q \"Please run 'ppz subs read'\""
ALERT_COUNT_PRE=$(ppz_s reread share-inbox.stdout --raw 2>/dev/null | grep -o "Please run 'ppz subs read'" | wc -l)
echo "first_alert_fired: yes"

# Recycle daemon: stop + start + login. Same shape as the stdin
# restart test, exercising the inbox-alerts forwarder's redial.
ppz_s daemon stop  >/dev/null
ppz_s daemon start >/dev/null
ppz_s daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

# Post-recycle: another .inbox publish should produce a *new* alert
# write. Without forwardInboxAlerts redialling, the pump never sees
# msg-2 → no second alert → count stays at $ALERT_COUNT_PRE.
ppz_b send share-inbox.inbox "msg-2" >/dev/null
wait_for 60 "[ \"\$(ppz_s reread share-inbox.stdout --raw 2>/dev/null | grep -c \"Please run 'ppz subs read'\")\" -gt $ALERT_COUNT_PRE ]"
ALERT_COUNT_POST=$(ppz_s reread share-inbox.stdout --raw 2>/dev/null | grep -o "Please run 'ppz subs read'" | wc -l)
[[ "$ALERT_COUNT_POST" -gt "$ALERT_COUNT_PRE" ]] && echo "alert_fired_again: yes" || echo "alert_fired_again: no"
