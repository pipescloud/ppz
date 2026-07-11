#!/usr/bin/env bash
# Server-side payload cap. The composer sets maxlength=2000 client-side, but a
# direct POST must not be able to publish an arbitrarily large message — the
# handler rejects an over-length payload (400) while a normal one still sends.
. /tests/lib/common.sh
auth_as_foo

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create desk >/dev/null
ppz_a pipe create general >/dev/null

BIG=$(printf 'a%.0s' $(seq 1 3000))
big_body=$(jq -nc --arg p "$BIG" '{kind:"pipe",target:"general",payload:$p,as:"desk"}')

echo "oversize: $(curl_server '/orgs/alpha/chat/send' -X POST -H 'Content-Type: application/json' \
  -d "$big_body" -o /dev/null -w '%{http_code}')"
echo "normal: $(curl_server '/orgs/alpha/chat/send' -X POST -H 'Content-Type: application/json' \
  -d '{"kind":"pipe","target":"general","payload":"hi","as":"desk"}' -o /dev/null -w '%{http_code}')"
