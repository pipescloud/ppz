#!/usr/bin/env bash
# RED → GREEN: a `ppz command` that a pty session already received and
# executed must NOT be delivered a second time when a new pty session is
# started on the same handle.
#
# Bug (AR#17): forwardStdin opens its Read with NoAdvance=true, so the
# daemon's cursor for <handle>.stdin never moves (internal/daemon/read.go
# :365) and every Read re-drains the whole retained window from
# info.State.FirstSeq (read.go:229). The only thing suppressing that
# replay is seenIDRing (internal/cli/terminal.go:1503) — an in-memory,
# in-process ring. A NEW `terminal share` process starts with an empty
# ring, so every retained .stdin message is written into the fresh PTY
# master verbatim (terminal.go:812). Retention is 24h
# (internal/server/streams.go:27), which is why the field report saw a
# /compact issued at 09:59 replayed 22.5 hours later.
#
# Repro is the reported one: share a handle, issue one command, kill the
# session, then resume the same handle in a new share process.
#
# Nothing here compacts anything: the wrapped child is a bash line
# reader standing in for any agent harness, and CMD1 is an opaque
# marker. What is under test is transport-level and harness-agnostic —
# an already-consumed .stdin message re-entering a fresh PTY.
#
# The child prints "ran:<line>" per SUBMITTED line, so the assertion
# counts executions, not echoes: the tty echo of replayed bytes carries
# no "ran:" prefix. `--newline` splits the command into two envelopes
# (instruction, then the submit byte) exactly as the operator's `ppz
# command` did, so "ran:CMD1" in session 2 proves the replay
# self-submits rather than merely painting the screen.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

# Prints "up" on start: the only barrier that actually proves a share
# process reached the point of following .stdin. `ppz status` does NOT —
# it reports the daemon's current handle, which `set handle` makes true
# before session 2 exists.
CHILD='echo up; while IFS= read -r line; do echo "ran:$line"; [ "$line" = QUIT ] && break; done'
stdout_lines() { ppz_a reread alex.stdout 2>/dev/null | tr -d '\r'; }

# --- session 1: the agent is alive and is given one command -----------
PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share alex -- bash -c "$CHILD" \
  </dev/null >/dev/null 2>&1 &
S1=$!
cleanup() {
  kill "$S1" "${S2:-}" 2>/dev/null || true
  wait "$S1" "${S2:-}" 2>/dev/null || true
}
trap cleanup EXIT
wait_for 50 "ppz_a ls 2>/dev/null | ls_normalize | matches '^alex.stdin'"

ppz_a command alex "CMD1" --newline >/dev/null
wait_for 50 "stdout_lines | matches 'ran:CMD1'"
echo "session1_executed: $(stdout_lines | grep -c 'ran:CMD1')"

# --- the agent is killed ---------------------------------------------
kill "$S1" 2>/dev/null || true
wait "$S1" 2>/dev/null || true

# --- session 2: resume the SAME handle in a fresh share process -------
ppz_a set handle alex >/dev/null
PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz terminal share -- bash -c "$CHILD" \
  </dev/null >/dev/null 2>&1 &
S2=$!

# Barrier: session 2's child is running (two "up" lines on the pipe: one
# per session). Ordering here is the whole test. The replay this pins
# only happens when the cursor covers the ENTIRE retained window at the
# moment the host connects: with something newer outstanding the daemon
# takes its retained-drain path, which sets the live consumer's start
# sequence correctly and hides the defect. So nothing may be published
# to .stdin between session 1's command and session 2 connecting —
# publishing the marker first is what made an earlier version of this
# scenario pass against a daemon that replays.
wait_for 100 "stdout_lines | grep -c '^up$' | grep -qx 2"
sleep 2

# Any replay has landed by now. 1 = the command ran only when it was
# issued; 2 = it was replayed into an agent that was never asked.
echo "cmd1_executions_total: $(stdout_lines | grep -c 'ran:CMD1')"

# The other half of the contract, asserted after the fact: a fix must
# not buy silence by going deaf to the pipe.
ppz_a command alex "PING" --newline >/dev/null
wait_for 50 "stdout_lines | matches 'ran:PING'"
echo "session2_live_input_works: $(stdout_lines | grep -c 'ran:PING')"

ppz_a command alex "QUIT" --newline >/dev/null
wait_for 50 "! kill -0 $S2 2>/dev/null"
