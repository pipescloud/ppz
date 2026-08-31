#!/usr/bin/env bash
# ACL Phase 1: a service account is a real principal.
#
# foo (owner of alpha) creates the service account `builder-bot` and
# mints a key for it. The key is created_by foo but acts_as the
# service, so everything it creates must attribute to builder-bot —
# not to foo. That divergence is the whole point of service accounts:
# an agent's work is its own, and an ACL grant naming builder-bot
# governs it.
#
# Also pins the display rule: usernames are stored scoped as
# "<org>/<name>" (users.username is globally unique) but every surface
# shows the bare name.
#
# RED until Phase 1 lands.
. /tests/lib/common.sh

api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sS -X "$method" "$PPZ_SERVER_URL$path" \
      -H "Authorization: Bearer $(key_alpha)" \
      -H "Content-Type: application/json" -d "$body"
  else
    curl -sS -X "$method" "$PPZ_SERVER_URL$path" \
      -H "Authorization: Bearer $(key_alpha)"
  fi
}

echo "--- create service account ---"
api POST /api/v1/svc '{"name":"builder-bot"}' | jq -r '.service.name // .error'

echo "--- list shows the bare name ---"
api GET /api/v1/svc | jq -r '.services[].name'

echo "--- mint a key for it ---"
SVC_KEY=$(api POST /api/v1/svc/builder-bot/keys | jq -r '.key // empty')
if [[ -z "$SVC_KEY" ]]; then echo "no key returned"; exit 1; fi

echo "--- the service key acts as the service, not as foo ---"
ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$SVC_KEY" >/dev/null
ppz_a source create deploybot >/dev/null
ppz_a ls --json | jq -c '{handle, creator}'
