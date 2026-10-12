#!/usr/bin/env bash
# Any member may create and delete from the Pipes tab, as with the CLI.
# Once the org enforces ACLs, deleting needs admin on every pipe removed
# (the check `ppz pipe set` applies). Seed: foo owns alpha, bar is a
# plain member.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create ops >/dev/null
ppz_a pipe create ops.deploy-log >/dev/null
ppz_a acl enforce on >/dev/null

post() { curl_server "/orgs/alpha$1" -X POST "${@:2}" -o /dev/null -w '%{http_code}'; }

auth_as_bar
echo "bar deletes foo's pipe: $(post /pipes/delete -d source=ops -d name=deploy-log)"
echo "bar deletes foo's source: $(post /sources/delete -d source=ops)"
echo "bar creates a pipe: $(post /pipes -d kind=pipe -d name=bar-notes)"

auth_as_foo
echo "foo deletes the pipe: $(post /pipes/delete -d source=ops -d name=deploy-log)"
echo "foo deletes the source: $(post /sources/delete -d source=ops)"
