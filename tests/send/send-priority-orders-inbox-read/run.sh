#!/usr/bin/env bash
# Priority ordering end-to-end: two unset (≡ medium) messages, then one
# `--priority high`. A single `ppz read` drain delivers the high message
# first, then the two mediums in ARRIVAL order — the daemon sorts with
# sort.SliceStable, so intra-tier FIFO is observable here: a regression
# to an unstable sort, a sort call misplaced relative to the Skip/Limit
# trim, or a forgotten sort call all break the first/second row order.
#
# The high row carries the advisory `P1 ` badge; the unset rows render
# byte-identically to pre-priority output (no badge).
#
# `reread` shares the daemon's handleRead path, so the forensic verb
# delivers the same priority order.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

ppz_a send chat.inbox "first" >/dev/null
ppz_a send chat.inbox "second" >/dev/null
ppz_a send chat.inbox "urgent" --priority high >/dev/null
wait_for 20 "ppz_a ls | grep -q urgent" >/dev/null

echo "--- read: high first, then FIFO mediums ---"
ppz_a read chat.inbox

echo "--- reread: same priority order (shared daemon path) ---"
ppz_a reread chat.inbox
