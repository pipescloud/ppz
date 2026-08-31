#!/usr/bin/env bash
# Logging in to an org that is ALREADY enforcing.
#
# Every other enforcement scenario turns enforcement on *after* login,
# which routes through the forced-refresh path. Nothing covered a fresh
# login into an org that already enforces — where the very first
# credential is the restrictive compiled one.
#
# If the daemon does not record the org's enforcement state at login, it
# cannot tell a denied stream enumeration from a transport fault, and
# `ppz ls` fails hard instead of degrading. Not a blip either: the
# refresh loop first fires near credential expiry, so the wrong value
# would stand for essentially the whole first credential lifetime.
. /tests/lib/common.sh

# Enforce first, using daemon A.
ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create ops >/dev/null
ppz_a pipe create ops.deploy-log >/dev/null
ppz_a acl enforce on >/dev/null

# Daemon B logs in fresh, into an org already enforcing.
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null

echo "--- ls degrades rather than failing ---"
if ppz_b ls >/dev/null 2>&1; then echo "ls=ok"; else echo "ls=FAILED"; fi

echo "--- and still lists the pipes it can see ---"
ppz_b ls 2>/dev/null | awk '/deploy-log/ {print $2}' | head -1
