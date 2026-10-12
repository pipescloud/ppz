#!/usr/bin/env bash
# The Pipes tab offers "purge" on rows that hold messages (auto-provisioned
# pipes included). Purging empties the buffer, keeps the pipe, and is
# audited as pipe.purge via web.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create desk >/dev/null
ppz_a send desk.inbox "one" >/dev/null
ppz_a send desk.inbox "two" >/dev/null
wait_for 20 "ppz_a reread desk.inbox | grep -q two" >/dev/null

auth_as_foo
offers() { curl_server "/orgs/alpha/pipes" | grep -oE 'data-purge-pipe="[^"]+"' | sort; }
echo "--- before ---"
offers
echo "purge: $(curl_server '/orgs/alpha/pipes/purge' -X POST -d source=desk -d name=inbox -o /dev/null -w '%{http_code}')"
echo "--- after ---"
offers
echo "buffered: $(ppz_a reread desk.inbox | grep -cE 'one|two')"
echo "pipe still listed: $(ppz_a ls | ls_normalize | cut -d' ' -f1 | grep -c '^desk.inbox$')"
echo "--- audit ---"
curl_server "/orgs/alpha/audit" | tr '\n' ' ' \
  | grep -oE 'data-audit-action="pipe.purge"[[:space:]]+data-audit-target="[^"]+"[[:space:]]+data-audit-actor="[^"]+"[[:space:]]+data-audit-via="[^"]+"[[:space:]]+data-audit-delta="[^"]+"' \
  | sed -E 's/data-audit-[a-z]+="([^"]+)"/\1/g; s/[[:space:]]+/ /g'
