#!/usr/bin/env bash
# REGRESSION GUARD: re-provisioning must not silently revert a `pipe set`.
#
# Making retention mutable meant switching stream provisioning to
# CreateOrUpdateStream. That cuts both ways: every path that re-provisions
# with the BUILT-IN DEFAULTS now overwrites a tuned stream instead of
# no-opping on it, the same bug class in the opposite direction.
#
# Bare `ppz terminal share` re-provisions the complete pty pipe set on
# EVERY invocation, so it is the one users would hit constantly — and it
# hits precisely the auto-provisioned pipes this feature exists to make
# configurable. After the share, postgres would still say max_msgs=3
# while JetStream had gone back to 5000.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

ppz_a pipe set chat.inbox --max-msgs=3 >/dev/null

# Promotes chat message→pty and re-provisions inbox/stdin/stdout/system/…
ppz_a terminal share -- printf "one" >/dev/null 2>&1
echo "share_rc: $?"

for i in 1 2 3 4 5; do
  ppz_a send chat.inbox "msg-$i" >/dev/null
done
wait_for 20 "ppz_a reread chat.inbox --json | jq -r '.payload' | grep -q msg-5" >/dev/null

# BUFFERED (col $3) must still be the configured 3, not the default 5000.
ppz_a ls | ls_normalize | awk '$1 == "chat.inbox" {print "buffered="$3}'
