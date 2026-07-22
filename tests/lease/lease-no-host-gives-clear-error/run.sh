#!/usr/bin/env bash
# RED: acquiring a lease on a terminal whose host isn't answering — no live
# `terminal share` process running the lease manager to consume .system — must
# fail with a CLEAR, accurate error (E_LEASE_NO_HOST, exit 28). Previously it
# returned E_DAEMON_TIMEOUT (exit 26), which blames the caller's *local* daemon
# ("retry, or run 'ppz daemon restart'") when the daemon is perfectly healthy —
# the real cause is a remote/absent/old host that never grants. This is the
# exact misleading error a user hit acquiring against an agent whose share
# process predated the terminal-control feature.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

# Provision a pty source (creates .system) but leave NO live host: the share's
# child exits immediately, so the source + streams persist while no lease
# manager consumes .system.
ppz_a terminal share ghost -- true >/dev/null 2>&1
wait_for 20 "ppz_a ls 2>/dev/null | ls_normalize | grep -q '^ghost.system'" >/dev/null

# Acquire against the hostless terminal: the acquire publishes, but nothing
# grants, so the CLI times out waiting for the lease-state.
PPZ_CURRENT_HANDLE=operator ppz_a terminal lease ghost 60s >/tmp/leasenohost.out 2>&1
echo "rc: $?"
grep -oE 'E_[A-Z_]+' /tmp/leasenohost.out | head -1
