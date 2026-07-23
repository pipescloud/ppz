#!/usr/bin/env bash
# RED — docs/specs/schedule.md. `ppz send <target> <payload> --at +2s`
# registers a durable one-off schedule on the server. At the requested
# instant the SERVER publishes the message (creator's handle stamped as
# sender, schedule_id marker on the envelope) and the schedule row
# disappears from `schedule ls`.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create bob >/dev/null
ppz_a source create alice >/dev/null   # current handle → alice (the sender)

out=$(ppz_a send bob "delayed hello" --at +2s 2>&1)
echo "send-exit=$?"
echo "$out" | grep -oE 'scheduled id=[a-f0-9]{8} to=bob\.inbox next=[^ ]+' | head -1

# The fire must land in bob.inbox within a few seconds — published by
# the server-side scheduler, not this daemon.
wait_for 100 'ppz_a reread bob.inbox -l 1 --bare 2>/dev/null | grep -q "delayed hello"'
echo "fired=$?"
ppz_a reread bob.inbox -l 1 --bare

# Envelope: sender is the creator's handle at creation time; the
# schedule_id marker distinguishes scheduled messages from live sends.
row=$(ppz_a reread bob.inbox -l 1 --json)
echo "$row" | grep -o '"sender":"alice"'
if echo "$row" | matches '"schedule_id":"'; then echo "schedule_id=present"; fi

# Fired one-offs leave the table (no STATUS column, no tombstone).
echo "rows-after=$(ppz_a schedule ls | wc -l | tr -d ' ')"
