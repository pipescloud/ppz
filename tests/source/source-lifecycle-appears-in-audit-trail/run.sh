#!/usr/bin/env bash
# Sources have their own lifecycle, and until now only the PIPES hanging
# off them were audited. `ppz source create` / `ppz source destroy` left
# no trace at all, so the trail could show a pipe created under a source
# nobody could see had been made — and a `source destroy`, which CASCADEs
# away every pipe under it, was the single most destructive org operation
# with no row of its own.
#
# The target is the source's PATH (manifold-qualified), matching how pipe
# rows address their target, so a namespaced source can't be confused
# with a root one of the same handle.
#
# RED until source.create / source.destroy are recorded.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null
ppz_a source destroy chat >/dev/null

echo "--- source actions, oldest first ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT action, target_type, target FROM audit_events
   WHERE action LIKE 'source.%'
   ORDER BY created_at ASC, action ASC
" | sed 's/|/ /g'

echo "--- create records the kind, destroy records what was lost ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT action, coalesce(after::text, before::text) FROM audit_events
   WHERE action LIKE 'source.%'
   ORDER BY created_at ASC, action ASC
" | sed 's/|/ /g'

echo "--- attributed to the key's principal, via the key ---"
PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -tAc "
  SELECT u.username, (e.actor_api_key_id IS NOT NULL)
    FROM audit_events e JOIN users u ON u.id = e.actor_user_id
   WHERE e.action = 'source.create'
" | sed 's/|/ /g'
