#!/usr/bin/env bash
# GUARD: the upgrade must be idempotent. Bare `ppz terminal share` twice on the
# same source — first upgrades message→pty, second runs against an already-pty
# source — must BOTH exit 0. The upgrade path must not choke on E_SOURCE_TAKEN
# or re-provisioning existing streams. Locks the contract so the GREEN
# implementation makes the upgrade a no-op when the source is already pty.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null

# First bare share: message → pty upgrade.
ppz_a terminal share -- printf "one" >/dev/null 2>&1
echo "first_rc: $?"

# Second bare share: source is already pty; must be a clean no-op.
ppz_a terminal share -- printf "two" >/dev/null 2>&1
echo "second_rc: $?"

# System pipe still present after the idempotent second run.
if ppz_a ls | ls_normalize | matches '^chat.system'; then
  echo "system_pipe: present"
else
  echo "system_pipe: missing"
fi
