#!/usr/bin/env bash
# Each sub-nav tab (pipes / users / keys) is its own URL, renders the
# shared org header + the same three-link sub-nav, marks the active
# tab with `data-active-tab`, and includes ONLY its own section.
# Lets the user deep-link to / refresh on any view without losing
# place, and keeps the page focused.
. /tests/lib/common.sh
auth_as_foo

org_id="$(cat /seed/org-alpha.txt)"

# A small helper: print the tab assertions for one URL.
check_tab() {
  local url="$1" tab="$2"
  local page; page="$(curl_server "$url")"
  echo "--- $url ($tab tab) ---"

  # Three sub-nav links must always be present.
  printf '%s' "$page" | grep -oE 'data-tab="pipes"' | head -1
  printf '%s' "$page" | grep -oE 'data-tab="users"' | head -1
  printf '%s' "$page" | grep -oE 'data-tab="keys"'  | head -1

  # Exactly one tab marked active.
  local active
  active="$(printf '%s' "$page" | grep -oE 'data-active-tab="[^"]+"' | head -1 | sed -E 's/data-active-tab="([^"]+)"/\1/')"
  echo "active-tab=$active"

  # Only the active section renders.
  for s in pipes users keys; do
    if printf '%s' "$page" | matches -E "id=\"section-$s\""; then
      echo "section-$s=present"
    else
      echo "section-$s=absent"
    fi
  done
}

check_tab "/orgs/$org_id/pipes" pipes
check_tab "/orgs/$org_id/users" users
check_tab "/orgs/$org_id/keys"  keys
