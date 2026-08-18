#!/usr/bin/env bash
# Pins that the inbox-alerts follow is invalidated and redialled
# when the share-side daemon's NATS connection is swapped out via
# logout/login (same-process), not just process restart.
#
# Existing -inbox-alerts-survives-share-daemon-restart only covers
# process restart, which already worked transitively through
# Scanner-EOF redial. The NC-swap (no-restart) path is a separate
# code path: it requires followRegistry.closeAll to be wired into
# swapNC, and handleRead to register conns up front. A regression
# that broke registration for the inbox channel specifically (e.g.,
# a gating condition on req.Channel) would slip through both the
# restart variant of this test AND the stdin variants — only this
# cross-product exposes it.
. /tests/lib/common.sh

ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

HOME_S=/tmp/share-inbox-logout
rm -rf "$HOME_S"; mkdir -p "$HOME_S"
SOCK_S=$HOME_S/daemon.sock

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

ppz_s terminal share share-inbox-logout -- bash -c 'stty -icanon -echo 2>/dev/null; cat' </dev/null >/dev/null 2>&1 &
SHARE_PID=$!

wait_for 50 "ppz_s ls 2>/dev/null | grep -q '^share-inbox-logout.stdout'"

# Pre-recycle: alert fires on first .inbox message, count via --raw +
# grep -o so concatenated payloads don't hide additional matches.
ppz_b send share-inbox-logout.inbox "msg-1" >/dev/null
wait_for 50 "ppz_s reread share-inbox-logout.stdout --raw 2>/dev/null | grep -q \"Please run 'ppz subs read'\""
ALERT_COUNT_PRE=$(ppz_s reread share-inbox-logout.stdout --raw 2>/dev/null | grep -o "Please run 'ppz subs read'" | wc -l)
echo "first_alert_fired: yes"

# NC swap WITHOUT process restart. daemon_same_pid: yes is the
# canary that we're really exercising the swapNC path.
PID_BEFORE=$(cat "$HOME_S/daemon.pid")
ppz_s daemon logout >/dev/null
sleep 0.3
ppz_s daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
PID_AFTER=$(cat "$HOME_S/daemon.pid")
[[ "$PID_BEFORE" = "$PID_AFTER" ]] && echo "daemon_same_pid: yes" || echo "daemon_same_pid: no"

ppz_b send share-inbox-logout.inbox "msg-2" >/dev/null
wait_for 60 "[ \"\$(ppz_s reread share-inbox-logout.stdout --raw 2>/dev/null | grep -o \"Please run 'ppz subs read'\" | wc -l)\" -gt $ALERT_COUNT_PRE ]"
ALERT_COUNT_POST=$(ppz_s reread share-inbox-logout.stdout --raw 2>/dev/null | grep -o "Please run 'ppz subs read'" | wc -l)
[[ "$ALERT_COUNT_POST" -gt "$ALERT_COUNT_PRE" ]] && echo "alert_fired_again: yes" || echo "alert_fired_again: no"
