#!/usr/bin/env bash
# The Security tab, enforcement off: the state every org lands in on
# upgrade. It must say plainly that nothing is enforced and show what
# turning it on would take away, because that is the whole point of
# making enforcement opt-in rather than flipping it globally.
. /tests/lib/common.sh
auth_as_foo

org_id="$(cat /seed/org-alpha.txt)"

# A pty source gives the preview a shared terminal to warn about; a
# collared user pipe gives it an owner-only case.
curl_server "/api/v1/sources" -X POST -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $(key_alpha)" -d '{"handle":"alice","kind":"pty"}' -o /dev/null
curl_server "/api/v1/sources/alice/pipes" -X POST -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $(key_alpha)" -d '{"name":"notes"}' -o /dev/null

page="$(curl_server "/orgs/$org_id/security")"

echo "--- the tab is reachable and reports the off state ---"
printf '%s' "$page" | matches -F 'data-acl-state="off"' && echo "state=off" || echo "state=MISSING"

echo "--- it offers to enable, not to disable ---"
printf '%s' "$page" | matches -F 'value="on"' && echo "enable-form=present" || echo "enable-form=missing"

echo "--- the preview names the shared terminal that would become private ---"
printf '%s' "$page" | tr '\n' ' ' | matches -E 'Shared terminals that become private:.*alice' \
  && echo "preview-terminal=present" || echo "preview-terminal=missing"

echo "--- and the collared pipe that becomes owner-only ---"
printf '%s' "$page" | tr '\n' ' ' | matches -E 'owner-only:.*alice\.notes' \
  && echo "preview-collared=present" || echo "preview-collared=missing"

echo "--- no rights table while nothing is enforced ---"
printf '%s' "$page" | matches -F 'id="acl-rights"' && echo "rights-table=present (BUG)" || echo "rights-table=absent"
