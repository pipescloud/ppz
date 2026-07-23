#!/usr/bin/env bash
# RED → GREEN: the PTY alert pump must fire when ANY subscribed pipe
# has unread, not just the auto-subscribed <handle>.inbox.
#
# Today's forwardInboxAlerts (terminal.go) opens an IPC Follow over
# the inbox channel specifically (Channel: "inbox"), so a message
# delivered to a subscribed ROOM does not reach the pump and the
# alert never fires. After the source rewire onto `ppz subs wait`
# (which blocks on the per-session subscription set as a whole),
# room messages produce the same alert as inbox messages.
#
# Mirrors the share-inbox-alerts-survives-* fixtures' wrapped-cat
# shape: every byte that lands on the PTY slave (whether from
# stdin-relay or from the alert pump's direct write) gets bounced
# to the slave's write side → master → publisher → <handle>.stdout,
# so alert-pump injections are observable via `reread --raw`.
. /tests/lib/common.sh

ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

HOME_S=/tmp/share-subs-room
rm -rf "$HOME_S"; mkdir -p "$HOME_S"
SOCK_S=$HOME_S/daemon.sock

# Dial idle + cooldown to sub-second so the alert window fits well
# inside the harness 30s ceiling and we don't have to wait the
# 15s/30s production defaults.
export PPZ_TERMINAL_INBOX_IDLE_MS=200
export PPZ_TERMINAL_INBOX_COOLDOWN_MS=200

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

# Daemon-B provisions a separate "chat room" source (room-x). The
# room is collared under its own handle — the share daemon will
# subscribe to room-x.inbox to monitor messages addressed to the
# room, distinct from share-subs-room.inbox (the share's own auto-
# subscribed inbox).
ppz_b source create room-x >/dev/null

# Pre-subscribe BEFORE starting the share. The forwardInboxAlerts
# loop snapshots the subscription set on each SubsWait call; a sub
# added AFTER the call is already in flight won't reach it until
# the next iteration (which only fires when the current wait
# returns). To exercise "wakes on a subscribed non-inbox pipe", the
# room sub has to be in place before the share's pump issues its
# first SubsWait.
PPZ_SESSION=share-subs-room ppz_s subs add room-x.inbox >/dev/null

ppz_s terminal share share-subs-room -- bash -c 'stty -icanon -echo 2>/dev/null; cat' </dev/null >/dev/null 2>&1 &
SHARE_PID=$!

wait_for 50 "ppz_s ls 2>/dev/null | grep -q '^share-subs-room.stdout'"

# Send to room-x.inbox, NOT share-subs-room.inbox. With the old
# inbox-only Follow source, a message to a non-self inbox never
# reached the pump → wait_for times out
# (`alert_fired_for_room: no` → expected.txt diff fails). With the
# subs-wait source, room-x.inbox is in the subscription set and the
# alert fires within the idle window.
ppz_b send room-x.inbox "alert me about a room message" >/dev/null
wait_for 60 "ppz_s reread share-subs-room.stdout --raw 2>/dev/null | grep -q \"Please run 'ppz subs read'\""

if ppz_s reread share-subs-room.stdout --raw 2>/dev/null | matches "Please run 'ppz subs read'"; then
  echo "alert_fired_for_room: yes"
else
  echo "alert_fired_for_room: no"
fi
