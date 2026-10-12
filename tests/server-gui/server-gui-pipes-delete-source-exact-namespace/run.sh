#!/usr/bin/env bash
# Source handles are unique per namespace, not per org. Deleting
# team-a.bot from the Pipes tab must leave the root `bot` alone.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
auth_as_foo
post() { curl_server "/orgs/alpha$1" -X POST "${@:2}" -o /dev/null -w '%{http_code}'; }

echo "create root bot: $(post /pipes -d kind=source -d name=bot)"
echo "create team-a.bot: $(post /pipes -d kind=source -d namespace=team-a -d name=bot)"
echo "delete team-a.bot: $(post /sources/delete -d source=team-a.bot)"
ppz_a ls | ls_normalize | cut -d' ' -f1 | grep -E '(^|\.)bot\.' | sort
