#!/usr/bin/env bash
# RED → GREEN: after the wrapped agent reads its subs (cursor advance),
# a stale pending bit must not fire one final redundant alert.
#
# Bug: streamForwardSubsAlertsOnce re-arms the pump's pending bit every
# ~250ms while a message sits unread (level-triggered subs wait). The
# agent's `ppz subs read` advances the cursor but publishes nothing on
# a subscribed subject, so the in-flight subs wait BLOCKS rather than
# returning the empty reply ObserveSubsClear (#119) listens for. A
# pending bit armed before the read survives it and, once
# idle + cooldown pass, the pump injects a redundant
# "Please run 'ppz subs read'" nag for a message already handled —
# user-observed on v0.46.1.
#
# Determinism: pending is only false in the ≤250ms gap between an alert
# firing (ReadyAlert consumes it) and the subs-wait loop's next
# level-triggered re-arm. A read landing in that gap leaves no stale
# pending and the buggy build false-passes. So we choreograph: observe a
# nag fire (alert count increase), sleep 500ms (> the 250ms re-arm
# throttle, < the 2s cooldown), THEN read — guaranteeing the stale
# pending exists at cursor-advance time on a buggy build, while the
# 2s cooldown guarantees the redundant nag has NOT yet fired when the
# baseline count is captured.
#
# Wrapped child is `cat` in raw mode (same shape as the
# share-inbox-alerts-survives-* fixtures): every alert-pump injection
# bounces slave→master→publisher onto <handle>.stdout, making alerts
# countable via `reread <handle>.stdout`.
. /tests/lib/common.sh

ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

HOME_S=/tmp/share-norenag
rm -rf "$HOME_S"; mkdir -p "$HOME_S"
SOCK_S=$HOME_S/daemon.sock

# Idle short so alerts start fast; cooldown 2s so the pending bit sits
# armed for ~87% of each nag cycle and the post-read quiet window can
# bracket exactly one would-be redundant fire. Total stays well inside
# the harness 30s ceiling.
export PPZ_TERMINAL_INBOX_IDLE_MS=200
export PPZ_TERMINAL_INBOX_COOLDOWN_MS=2000
# Ceiling pinned to the base so repeat alerts keep a FLAT 2s cadence.
# Repeats normally back off geometrically (see CooldownMax in
# terminal_subs_alert.go), and this scenario deliberately syncs past
# TWO alerts before reading — so unacked would be 2 and the next
# would-be nag 4s out, not 2s. That narrows the DETECTION margin: the
# redundant nag this fixture exists to catch would land at [T+4s, T+5s]
# against a 5s quiet window sampled at ~T+5.5s, leaving ~0.5s for the
# PTY->publisher->reread round trip. A regressed ConfirmUnread could
# then false-PASS. The flat cadence restores the ~2.5s cushion the
# quiet window was sized for.
export PPZ_TERMINAL_INBOX_COOLDOWN_MAX_MS=2000

ppz_s() { PPZ_HOME=$HOME_S PPZ_IPC_SOCKET=$SOCK_S ppz "$@"; }

alert_count() {
  ppz_s reread share-norenag.stdout --raw 2>/dev/null \
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

ppz_s terminal share share-norenag -- bash -c 'stty -icanon -echo 2>/dev/null; cat' </dev/null >/dev/null 2>&1 &
SHARE_PID=$!

# No ^ anchor: `ls` rows lead with the NAMESPACE column, so an
# anchored pipe-path grep never matches (the survives-* fixtures carry
# the same anchored grep, but unguarded — it silently burns the wait
# and they recover; here it would hard-exit).
wait_for 50 "ppz_s ls 2>/dev/null | grep -q 'share-norenag.stdout'" \
  || { echo "share never came up"; exit 1; }

# One message → nag loop starts (one alert per cooldown window while
# unread — that's the intended level-triggered behaviour).
ppz_b send share-norenag.inbox "msg-1" >/dev/null
wait_for 50 "[ \"\$(alert_count)\" -ge 1 ]" \
  || { echo "first alert never fired"; exit 1; }
echo "first_alert_fired: yes"

# Synchronise to the nag cycle: catch the next fire, then give the
# subs-wait loop 500ms to re-arm pending past its 250ms throttle.
C_SYNC=$(alert_count)
wait_for 60 "[ \"\$(alert_count)\" -gt $C_SYNC ]" \
  || { echo "nag loop never re-fired"; exit 1; }
sleep 0.5

# Read as the share session — the wrapped agent's exact action. The
# cursor advances; nothing is published; the pump's blocked subs wait
# never returns an empty reply. Assert the read really drained msg-1
# so a silent read failure can't masquerade as a pass below.
READ_OUT=$(PPZ_SESSION=share-norenag ppz_s subs read 2>/dev/null)
echo "$READ_OUT" | matches "msg-1" && echo "subs_read_done: yes" || echo "subs_read_done: no"
ALERT_COUNT_AT_READ=$(alert_count)

# Quiet window: 5s ≫ cooldown (2s) + flush tick (1s) + idle (200ms).
# Any growth in the alert count is the redundant post-read nag.
sleep 5
ALERT_COUNT_POST=$(alert_count)
if [[ "$ALERT_COUNT_POST" -eq "$ALERT_COUNT_AT_READ" ]]; then
  echo "redundant_alert_after_read: no"
else
  echo "redundant_alert_after_read: yes ($ALERT_COUNT_AT_READ -> $ALERT_COUNT_POST)"
fi
