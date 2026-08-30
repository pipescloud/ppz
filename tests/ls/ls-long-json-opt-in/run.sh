#!/usr/bin/env bash
# CONTRACT: `ppz ls --json` must not change unless -l is also given.
#
# Agents parse this output; silently adding keys to the default form is a
# wire change for every existing consumer. So retention is opt-in in JSON
# exactly as it is in the table — the daemon does not even populate the
# fields unless the request asked for them.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null
ppz_a pipe create chat.archive >/dev/null
ppz_a pipe set chat.archive --max-msgs=500 >/dev/null

echo "--- default --json carries no retention keys ---"
ppz_a ls --json | jq -c 'select(.pipe == "archive") | [has("ttl_seconds"), has("max_msgs"), has("max_bytes")]'

echo "--- --json -l opts in ---"
ppz_a ls --json -l | jq -c 'select(.pipe == "archive") | {ttl_seconds, max_msgs, max_bytes}'
