#!/usr/bin/env bash
# RED → GREEN: the repeat-alert backoff ladder must reset when the
# wrapped agent actually consumes its messages — including when the
# read is OCCLUDED by a new arrival before the next fire attempt.
#
# Field-observed 2026-08-19 (zif share, v0.56.5): alert gaps climbed
# 5m → 10m → 20m while the agent read and replied to every message
# between alerts. Root cause: the ladder reset lives only in the
# negative-ConfirmUnread branch, which needs a fire attempt to catch
# the unread level at exactly zero. Fire attempts happen only at
# gate-open moments; with ongoing traffic there is always a fresh
# unread message by then, so consumption is never observed and unacked
# climbs monotonically for a fully responsive agent.
#
# Fix under test: consumption is derived from the per-pipe watermark
# (Total - Unread) in the snapshot ConfirmUnread already fetches; a
# watermark advance since the last alert resets the ladder.
#
# Choreography per cycle: detect the alert, read AS THE SHARE SESSION
# (cursor advances — the exact agent action), send a fresh message,
# then WAIT until that message is visible as unread in the session's
# subs snapshot and require the visibility to land at least 1s before
# the cooldown gate opens. That last step is what makes the run valid:
# if the message is visible before the gate opens and nothing reads
# it, no post-gate flush tick can ever see a zero level, so the
# legitimate suppressed-fire reset (the pre-fix down-edge path) is
# impossible BY CONSTRUCTION and the read stays occluded from the
# level-only view — exactly the field condition.
#
# This replaced a gap-shape heuristic that false-passed on the buggy
# build: JetStream delivery lag (~2s from `send` to the message
# appearing in the daemon's subs state) let a post-gate tick catch the
# level at zero in one cycle, silently resetting the ladder through
# the legitimate path, and the resulting suppress-then-refire gap was
# indistinguishable from an occluded base gap at these timescales.
# Hence also the 5s cooldown: wide enough that visibility reliably
# beats the gate.
#
# Discriminator (fire-to-fire, detection jitter ±~1s, $SECONDS
# resolution 1s):
#   gap alert2→alert3, fixed:  base 5s         → measured 5-7s
#   gap alert2→alert3, buggy:  2^2 rung = 10s  → measured 10-12s
#
# Wrapped child is `cat` in raw mode (same as the other alert
# fixtures): injections bounce slave→master→publisher onto
# <handle>.stdout, countable via `reread`.
. /tests/lib/common.sh

ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

HOME_S=/tmp/share-ladder-reset
rm -rf "$HOME_S"; mkdir -p "$HOME_S"
SOCK_S=$HOME_S/daemon.sock

export PPZ_TERMINAL_INBOX_IDLE_MS=200
export PPZ_TERMINAL_INBOX_COOLDOWN_MS=5000
export PPZ_TERMINAL_INBOX_COOLDOWN_MAX_MS=20000

ppz_s() { PPZ_HOME=$HOME_S PPZ_IPC_SOCKET=$SOCK_S ppz "$@"; }

alert_count() {
  ppz_s reread share-ladder-reset.stdout --raw 2>/dev/null \
    | grep -o "Please run 'ppz subs read'" | wc -l | tr -d ' '
}

# Unread level as the share session's fire-time confirm sees it (same
# snapshot family as IPCSubsList). Non-zero means a post-gate tick
# cannot suppress.
session_has_unread() {
  PPZ_SESSION=share-ladder-reset ppz_s subs ls 2>/dev/null \
    | awk 'NR>1 && $3 ~ /^[0-9]+$/ && $3 > 0 { found=1 } END { exit !found }'
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

ppz_s terminal share share-ladder-reset -- bash -c 'stty -icanon -echo 2>/dev/null; cat' </dev/null >/dev/null 2>&1 &
SHARE_PID=$!

# No ^ anchor: `ls` rows lead with the NAMESPACE column.
wait_for 50 "ppz_s ls 2>/dev/null | grep -q 'share-ladder-reset.stdout'" \
  || { echo "share never came up"; exit 1; }

ppz_b send share-ladder-reset.inbox "msg-1" >/dev/null
wait_for 60 "[ \"\$(alert_count)\" -ge 1 ]" \
  || { echo "first alert never fired"; exit 1; }
echo "first_alert_fired: yes"
T1=$SECONDS

# Two read→send→alert cycles. The read must drain (assert it, so a
# silent read failure can't fake the choreography); the send must land
# inside the cooldown so the read is occluded from the level-only view.
T2= ; T3=
for i in 2 3; do
  PREV=$(alert_count)
  T_ALERT=$SECONDS
  READ_OUT=$(PPZ_SESSION=share-ladder-reset ppz_s subs read 2>/dev/null)
  echo "$READ_OUT" | matches "msg-$((i-1))" \
    || { echo "cycle $i: subs read did not drain msg-$((i-1))"; exit 1; }
  ppz_b send share-ladder-reset.inbox "msg-$i" >/dev/null
  # Occlusion precondition: the new message must be VISIBLE as unread
  # in the session's subs state at least 1s before the gate opens
  # (alert time + 5s cooldown). Visibility before gate-open makes the
  # legitimate zero-level suppression impossible, so a base-cadence
  # alert below can only mean the watermark reset — not a race.
  wait_for 40 "session_has_unread" \
    || { echo "cycle $i: msg-$i never became visible"; exit 1; }
  if (( SECONDS - T_ALERT > 4 )); then
    echo "occlusion_missed: cycle $i visibility at +$((SECONDS - T_ALERT))s, gate opens at +5s"
    exit 1
  fi
  # 130 ticks = 13s: past the buggy build's 10s rung, so a climbed
  # ladder reaches the gap assertion instead of dying in a timeout.
  wait_for 130 "[ \"\$(alert_count)\" -gt $PREV ]" \
    || { echo "alert $i never fired"; exit 1; }
  echo "alert$i: yes"
  [[ $i -eq 2 ]] && T2=$SECONDS
  [[ $i -eq 3 ]] && T3=$SECONDS
done

GAP23=$(( T3 - T2 ))
# Fixed: base 5s → 5-7s measured. Buggy: 10s rung → 10-12s measured.
if [[ "$GAP23" -le 7 ]]; then
  echo "ladder_reset_on_read: yes"
  exit 0
fi

# Climbed gap23: distinguish a real regression from a single-window
# crediting delay before declaring failure. By design, a fire whose
# confirm errors keeps the previous baseline (at-least-once — pinned
# by ErrorFireKeepsConsumptionBaseline), so under host contention one
# window can legitimately miss its credit and pick it up at the next
# alert; observed once in CI-like conditions alongside a sibling
# fixture timing out. One more cycle separates the cases: a delayed
# credit resets at alert3 and alert4 arrives on the BASE gap; a real
# regression climbs to the 20s rung, which overruns the wait budget —
# "alert 4 never fired" — or lands far outside the base band.
PREV=$(alert_count)
READ_OUT=$(PPZ_SESSION=share-ladder-reset ppz_s subs read 2>/dev/null)
echo "$READ_OUT" | matches "msg-3" \
  || { echo "probe cycle: subs read did not drain msg-3"; exit 1; }
ppz_b send share-ladder-reset.inbox "msg-4" >/dev/null
wait_for 40 "session_has_unread" \
  || { echo "probe cycle: msg-4 never became visible"; exit 1; }
wait_for 130 "[ \"\$(alert_count)\" -gt $PREV ]" \
  || { echo "ladder_reset_on_read: no (gap23 ${GAP23}s, then alert 4 never fired within 13s; ladder climbed despite reads)"; exit 0; }
GAP34=$(( SECONDS - T3 ))
if [[ "$GAP34" -le 7 ]]; then
  # Stdout must match expected.txt byte-for-byte on every green path;
  # the deferred-credit detail goes to stderr (harness discards it).
  echo "deferred credit: gap23 ${GAP23}s, then base ${GAP34}s" >&2
  echo "ladder_reset_on_read: yes"
else
  echo "ladder_reset_on_read: no (gaps ${GAP23}s then ${GAP34}s; ladder climbed despite reads)"
fi
