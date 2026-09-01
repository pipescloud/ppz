#!/usr/bin/env bash
# ACL Phase 1 made service accounts real principals: they hold grants,
# own handles, and carry keys whose work attributes to the bot rather
# than to the human who minted it. That last property is exactly why
# their lifecycle needs auditing — a service account is a way for a
# person's action to stop looking like theirs, so the moment one is
# created, keyed, or destroyed has to be on the record.
#
# Targets carry the BARE name. Usernames are stored scoped as
# "<org>/<name>" because users.username is globally unique, but every
# other surface shows the bare name and the trail must match.
#
# The destroy is exercised on a SECOND, keyless service account, and
# that is not arbitrary: DeleteServiceAccount does a bare DELETE on
# `users`, so once a service account has had a key minted, its
# api_keys.principal_user_id FK blocks the delete permanently and the
# endpoint 500s. That is a pre-existing product bug, unrelated to the
# trail — this scenario routes around it rather than encoding it, so it
# starts passing on its own merits the day the delete is fixed.
#
# RED until svc.create / svc.key.mint / svc.destroy are recorded.
. /tests/lib/common.sh

api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sS -X "$method" "$PPZ_SERVER_URL$path" \
      -H "Authorization: Bearer $(key_alpha)" \
      -H "Content-Type: application/json" -d "$body" -o /dev/null
  else
    curl -sS -X "$method" "$PPZ_SERVER_URL$path" \
      -H "Authorization: Bearer $(key_alpha)" -o /dev/null
  fi
}

api POST /api/v1/svc '{"name":"builder-bot"}'
api POST /api/v1/svc/builder-bot/keys
api POST /api/v1/svc '{"name":"janitor-bot"}'
api DELETE /api/v1/svc/janitor-bot

echo "--- service-account actions, oldest first ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT action, target_type, target FROM audit_events
   WHERE action LIKE 'svc.%'
   ORDER BY created_at ASC
" | sed 's/|/ /g'

echo "--- minting the bot's key is attributed to the human who did it ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT u.username FROM audit_events e JOIN users u ON u.id = e.actor_user_id
   WHERE e.action = 'svc.key.mint'
"
