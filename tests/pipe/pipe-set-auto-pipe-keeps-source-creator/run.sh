#!/usr/bin/env bash
# Retuning an auto-pipe must not reassign who created it.
#
# `ppz pipe set` materialises a pipes row for auto-provisioned pipes on
# first override (they have no row otherwise, which is why their caps
# were previously unreachable). That row carries a created_by_user_id,
# and `ppz ls` renders it in the CREATOR column — where auto-pipes are
# supposed to inherit the SOURCE's creator.
#
# So the row must be stamped with the source's creator, not the person
# who changed the retention. Changing a cap is not creating a pipe, and
# "who changed it" is what the audit trail is for.
#
# foo (alpha-primary) creates the source; bar (alpha-secondary) retunes
# its inbox. CREATOR must still read foo.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)"  >/dev/null  # foo
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null  # bar

ppz_a source create chat >/dev/null          # source created by foo
ppz_b pipe set chat.inbox --max-msgs=3 >/dev/null   # retuned by bar

# Scoped to chat: `ppz ls` lists every source in the org, so an unscoped
# select turns leftover state from another scenario into a failure here.
ppz_a ls --json | jq -c 'select(.handle == "chat") | {handle, pipe, creator}'
