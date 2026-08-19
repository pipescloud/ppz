#!/usr/bin/env bash
# RED → GREEN: a message arriving after the agent consumed the pending
# episode must get a FRESH idle window, not the stale remainder of the
# window the previous message opened.
#
# Field-observed (rex share, v0.56.6, 2026-08-19): msg-1 armed the
# idle gate at 17:32:59; the agent's monitor read it at 17:33:12 (a
# read produces no down-edge, so pendingSince stayed stamped at
# msg-1's arrival); msg-2 landed 17:33:56; the stale gate opened at
# +60s and the wrapper injected at 17:34:01 — msg-2 got FIVE SECONDS
# of idle grace instead of 60. Same root as the #194 ladder bug on the
# other gate: consumption is invisible between fire attempts.
#
# Fix under test: at a fire attempt whose confirm shows the watermark
# advanced during the pending window AND whose surviving unread is
# young (< IdleAfter old), the injection is suppressed and the idle
# window restamped — the young message earns its own full window.
#
# Choreography (idle scaled to 5s): send msg-1, read it AS THE SHARE
# SESSION (the agent's exact action), send msg-2 immediately — msg-2
# is then ~4s young when the stale gate opens at ~+5s.
#   sample A (past the stale gate): buggy build has injected (count 1),
#     fixed build has suppressed (count 0)
#   sample B (past the fresh window): both builds show exactly 1 —
#     proving the deferral is not a drop, and pinning no double-fire.
. /tests/lib/common.sh

ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

HOME_S=/tmp/share-fresh-window
rm -rf "$HOME_S"; mkdir -p "$HOME_S"
SOCK_S=$HOME_S/daemon.sock

export PPZ_TERMINAL_INBOX_IDLE_MS=8000
# Cooldown far beyond the fixture's horizon: no injection precedes the
# one under test, so the cooldown gate must never participate.
export PPZ_TERMINAL_INBOX_COOLDOWN_MS=30000
export PPZ_TERMINAL_INBOX_COOLDOWN_MAX_MS=30000

ppz_s() { PPZ_HOME=$HOME_S PPZ_IPC_SOCKET=$SOCK_S ppz "$@"; }

alert_count() {
  ppz_s reread share-fresh-window.stdout --raw 2>/dev/null \
    | grep -o "Please run 'ppz subs read'" | wc -l | tr -d ' '
}

session_has_unread() {
  PPZ_SESSION=share-fresh-window ppz_s subs ls 2>/dev/null \
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

ppz_s terminal share share-fresh-window -- bash -c 'stty -icanon -echo 2>/dev/null; cat' </dev/null >/dev/null 2>&1 &
SHARE_PID=$!

wait_for 50 "ppz_s ls 2>/dev/null | grep -q 'share-fresh-window.stdout'" \
  || { echo "share never came up"; exit 1; }

# msg-1 arms the episode the instant the daemon's subs-wait wakes
# (~publish time), so the stale gate is ~T0+8s regardless of how long
# our own polling takes to SEE anything. Keep the window's CLI budget
# to the two essential calls — read msg-1, send msg-2 — with a fixed
# 0.5s delivery pause instead of poll loops (each poll is a full CLI
# round trip and this fixture's earlier 5s window drowned in them on
# a loaded host; the drain assert below catches a too-early read).
T0=$SECONDS
ppz_b send share-fresh-window.inbox "msg-1" >/dev/null
sleep 0.5
READ_OUT=$(PPZ_SESSION=share-fresh-window ppz_s subs read 2>/dev/null)
echo "$READ_OUT" | matches "msg-1" && echo "msg1_read: yes" \
  || { echo "msg1_read: no"; exit 1; }
# msg-2 must land LATE in the window — the field shape (msg-2 at
# second 57 of 60) and the deferral's own boundary. A message sent
# immediately after the read is nearly as old as the window when the
# gate opens, i.e. an OLD survivor, and the fix CORRECTLY fires for
# it at the gate (OldSurvivorFiresAtTheGate pins that). The first
# version of this fixture sent msg-2 at ~+1s and failed on the FIXED
# build in CI for exactly that reason. +5s of an 8s window leaves
# msg-2 ~3-4s young at the gate.
while (( SECONDS - T0 < 5 )); do sleep 0.2; done
ppz_b send share-fresh-window.inbox "msg-2" >/dev/null
# The choreography must land msg-2 with ≥2s of window left for it to
# become visible to the gate's confirm, or the run proves nothing —
# bail loud rather than false-pass either way.
if (( SECONDS - T0 > 6 )); then
  echo "choreography_too_slow: $((SECONDS - T0))s of the 8s window consumed"
  exit 1
fi
# Occlusion precondition (same construction as the ladder fixture):
# msg-2 must be VISIBLE as unread before the gate opens. If it is
# still invisible when the gate's confirm runs, the level reads zero
# and today's negative-confirm suppression grants the fresh window by
# accident — the buggy build false-passes. Visible-before-gate makes
# that path unreachable, so a sample-A zero can only mean the fix.
wait_for 60 "session_has_unread" \
  || { echo "msg-2 never became visible"; exit 1; }
if (( SECONDS - T0 > 7 )); then
  echo "occlusion_missed: msg-2 visibility confirmed at +$((SECONDS - T0))s, gate opens at +8s"
  exit 1
fi

# Sample A: 11s past msg-1 — the stale gate (~+8s) has opened and any
# injection has had ~2s to echo back through the wrapped cat, while
# the fixed build's fresh gate (~+16s) is still far off. Budget note:
# the samples are trimmed against the harness 30s ceiling — sample B
# dominates the runtime and leaves ~10s for daemon start, logins and
# share-up.
while (( SECONDS - T0 < 11 )); do sleep 0.2; done
A=$(alert_count)
if [[ "$A" -eq 0 ]]; then
  echo "suppressed_at_stale_gate: yes"
else
  echo "suppressed_at_stale_gate: no (the stale window fired for a seconds-old msg-2)"
fi

# Sample B: 20s past msg-1 — the fresh window (suppression ~+8.5s,
# +8s idle → fire ~+17s, echo ~+18s) has matured. Exactly one alert
# total: deferred, not dropped, not doubled.
while (( SECONDS - T0 < 20 )); do sleep 0.2; done
B=$(alert_count)
if [[ "$B" -eq 1 ]]; then
  echo "deferred_alert_fired_once: yes"
else
  echo "deferred_alert_fired_once: no (count $B)"
fi
