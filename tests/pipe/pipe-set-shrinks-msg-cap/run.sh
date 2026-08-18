#!/usr/bin/env bash
# `ppz pipe set <pipe> --max-msgs=N` retunes an ALREADY-PROVISIONED
# JetStream stream. This is the behavioural core of the feature: before
# `pipe set`, retention was fixed at create time and the provisioning
# path swallowed ErrStreamNameAlreadyInUse, so a changed config was a
# silent no-op.
#
# Create at the default 5000-msg cap, shrink to 5, publish 8, and assert
# the ring discarded the oldest 3.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null
ppz_a pipe create chat.ring >/dev/null

ppz_a pipe set chat.ring --max-msgs=5

for i in 1 2 3 4 5 6 7 8; do
  ppz_a send chat.ring "msg-$i" >/dev/null
done

wait_for 20 "ppz_a reread chat.ring --json | jq -r '.payload' | grep -q msg-8" >/dev/null

# Layout: PIPE UNREAD BUFFERED LAST PAYLOAD. BUFFERED (col $3) is the
# retained-message count. Should be 5, not 8.
ppz_a ls | ls_normalize | awk '$1 == "chat.ring" {print "buffered="$3}'
