#!/usr/bin/env bash
# RED → GREEN: repeat subs alerts must back off geometrically while the
# message stays unread, instead of re-nagging on a flat cooldown.
#
# Bug (user-observed): the alert cooldown is a FIXED interval, so an
# unread message the wrapped agent cannot action produces one injected +
# submitted turn per cooldown window, forever. The reported trigger is a
# session usage limit — the agent hits its limit with a message unread,
# the wrapper nags every 30s for the n hours until the limit resets, and
# the entire backlog then flushes as real turns, burning context window
# and token budget. At the old 30s cadence a 5-hour block queues ~600
# copies of the same nag.
#
# Nothing in the pump could see it: ConfirmUnread truthfully reports
# "still unread" on every fire (it IS unread), and the subs-wait loop
# re-arms pending every ~250ms regardless. The fix counts alerts
# injected since the last PROOF of consumption (a negative
# ConfirmUnread) and doubles the gap per unacknowledged alert up to a
# ceiling.
#
# Scaled-down cadence for the harness 30s ceiling: base 1s, ceiling 3s.
# Ladder gaps run 1s, 2s, 3s, 3s… so a 9s observation window admits ~4
# alerts. A flat-cooldown build fires every 1s — ~9 in the same window,
# well outside the bound asserted below.
#
# Wrapped child is `cat` in raw mode (same shape as the
# share-subs-alert-no-renag-after-read fixture): every alert-pump
# injection bounces slave→master→publisher onto <handle>.stdout, making
# alerts countable via `reread <handle>.stdout`.
. /tests/lib/common.sh

ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

HOME_S=/tmp/share-backoff
rm -rf "$HOME_S"; mkdir -p "$HOME_S"
SOCK_S=$HOME_S/daemon.sock

export PPZ_TERMINAL_INBOX_IDLE_MS=200
export PPZ_TERMINAL_INBOX_COOLDOWN_MS=1000
export PPZ_TERMINAL_INBOX_COOLDOWN_MAX_MS=3000

ppz_s() { PPZ_HOME=$HOME_S PPZ_IPC_SOCKET=$SOCK_S ppz "$@"; }

alert_count() {
  ppz_s reread share-backoff.stdout --raw 2>/dev/null \
    | grep -o "Please run 'ppz subs read'" | wc -l | tr -d ' '
}

cleanup() {
  PID=$(cat "$HOME_S/daemon.pid" 2>/dev/null || true)
  [[ -n "$PID" ]] && kill "$PID" 2>/dev/null || true
  kill "$SHARE_PID" 2>/dev/null || true
  wait "$SHARE_PID" 2>/dev/null || true
}
trap cleanup EXIT

ppz_s daemon start >/dev/null
ppz_s daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

ppz_s terminal share share-backoff -- bash -c 'stty -icanon -echo 2>/dev/null; cat' </dev/null >/dev/null 2>&1 &
SHARE_PID=$!

# No ^ anchor: `ls` rows lead with the NAMESPACE column, so an anchored
# pipe-path grep never matches.
wait_for 50 "ppz_s ls 2>/dev/null | grep -q 'share-backoff.stdout'" \
  || { echo "share never came up"; exit 1; }

# One message, never read — the usage-limit shape, where the agent
# cannot action what it has been told about.
ppz_b send share-backoff.inbox "msg-1" >/dev/null
wait_for 50 "[ \"\$(alert_count)\" -ge 1 ]" \
  || { echo "first alert never fired"; exit 1; }
echo "first_alert_fired: yes"

# Measure the repeat cadence from the first alert onward.
BASE=$(alert_count)
sleep 9
DELTA=$(( $(alert_count) - BASE ))

# Still nagging: a build that simply stopped alerting would satisfy the
# upper bound while silently dropping the message on the floor.
if [[ "$DELTA" -ge 2 ]]; then
  echo "alerts_continue: yes"
else
  echo "alerts_continue: no ($DELTA repeats in 9s)"
fi

# Bounded: ladder gaps 1s, 2s, 3s, 3s admit ~4 repeats in 9s. A flat 1s
# cooldown admits ~9.
if [[ "$DELTA" -le 5 ]]; then
  echo "repeat_alerts_bounded: yes"
else
  echo "repeat_alerts_bounded: no ($DELTA repeats in 9s; flat-cooldown cadence)"
fi
