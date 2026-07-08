#!/usr/bin/env bash
# Pins that priority sorting runs AFTER the -l/--skip arrival-window trim,
# not before. Send two unset (medium) messages then one --priority high,
# newest-last. `reread -l 2` must keep the newest TWO by ARRIVAL (second,
# urgent), THEN sort them high-first → [urgent, second]. "first" is
# excluded because it's the oldest.
#
# If the sort ran BEFORE the trim, it would float `urgent` to the front of
# all three, and -l 2 (keep last two) would then keep [first, second] —
# dropping the high-priority message entirely and showing the wrong pair.
# This is the exact regression the placement comment in read.go guards
# against, and it is unobservable without combining -l with priority.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

ppz_a send chat.inbox "first"                  >/dev/null   # oldest,  medium
ppz_a send chat.inbox "second"                 >/dev/null   # middle,  medium
ppz_a send chat.inbox "urgent" --priority high >/dev/null   # newest,  high
wait_for 20 "ppz_a ls | grep -q urgent" >/dev/null

echo "--- reread -l 2: arrival-window [second, urgent] then sorted high-first ---"
ppz_a reread chat.inbox -l 2
