#!/usr/bin/env bash
# Cross-tenant guard for every session route under /orgs/<id>: a login
# proves who you are, not that the org is yours. foo is a member of alpha
# only, so beta's tabs, pipe pages, terminal page/WS, key creation and
# invites must all 404 — same answer as a nonexistent org, so beta's existence doesn't
# leak. (Chat has its own scenario: server-gui-chat-non-member-404.)
. /tests/lib/common.sh

# Give beta a shared terminal and an uncollared pipe so the 404s below
# are the membership gate, not "nothing there".
ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_beta)" >/dev/null
ppz_a terminal share term1 -- printf "beta-secret" >/dev/null
ppz_a pipe create general >/dev/null
wait_for 20 "ppz_a read term1.stdout --json | jq -r '.payload' | grep -q beta-secret" >/dev/null

auth_as_foo
code() { curl_server "$@" -o /dev/null -w '%{http_code}'; }
for tab in pipes users keys audit security; do
  echo "tab $tab: $(code "/orgs/beta/$tab")"
done
echo "org redirect: $(code '/orgs/beta')"
echo "pipe page: $(code '/orgs/beta/sources/term1/pipes/stdout')"
echo "uncollared pipe page: $(code '/orgs/beta/pipes/general')"
echo "terminal page: $(code '/orgs/beta/sources/term1/terminal')"
echo "terminal ws: $(curl_server '/orgs/beta/sources/term1/terminal/ws' \
  -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: AAAAAAAAAAAAAAAAAAAAAA==' \
  --max-time 4 -o /dev/null -w '%{http_code}')"
echo "key create: $(code '/orgs/beta/keys' -X POST --data-urlencode 'label=intrusion')"
echo "invite create: $(code '/orgs/beta/invites' -X POST --data-urlencode 'username=bar')"
echo "invite revoke: $(code '/orgs/beta/invites/00000000-0000-0000-0000-000000000000/revoke' -X POST)"
echo "pipe create: $(code '/orgs/beta/pipes' -X POST -d kind=pipe -d name=intrusion)"
echo "pipe delete: $(code '/orgs/beta/pipes/delete' -X POST -d name=general)"
echo "pipe purge: $(code '/orgs/beta/pipes/purge' -X POST -d name=general)"
echo "source delete: $(code '/orgs/beta/sources/delete' -X POST -d source=term1)"
echo "intrusion keys in beta: $(PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc \
  "SELECT COUNT(*) FROM api_keys k JOIN accounts a ON a.id = k.account_id
    WHERE a.name = 'beta' AND k.label = 'intrusion'")"

# Sanity: foo's own org still works.
echo "own tab: $(code '/orgs/alpha/pipes')"
