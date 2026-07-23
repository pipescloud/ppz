#!/usr/bin/env bash
# RED — observability half of the 2026-06-11 wake-from-sleep incident
# (ppz-diag-20260611-073803.tgz).
#
# In that incident the daemon's JWT expired while the laptop slept, and
# for ~70s after wake every POST /api/v1/auth/exchange failed. The user
# saw E_SERVER_UNREACHABLE; the diagnostics bundle recorded NOTHING
# about those refresh failures — ensureNATS maps every non-401 refresh
# error to EServerUnreachable and discards the cause, and the refresh
# loop records no event on failure. The single fact that would have
# closed the investigation (WHY the exchange failed: DNS, dial timeout,
# TLS, refused) was unobservable.
#
# Contract pinned here: when a due refresh fails, `ppz diagnostics`
# must show a `refresh_error` event whose reason carries the underlying
# error text.
#
# Mechanics: the reliability overlay sets PPZ_NATS_JWT_TTL=120s on
# ppz-server, so the JWT minted at login expires 120s later and the
# daemon's refresh fires at exp-30s = login+90s. We stop ppz-server
# right after login, sleep past the fire point (plus a couple of the
# loop's 5s retry cycles), and inspect diagnostics — which is IPC-only
# and works with the server down, by design.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null

# Take the server down before the refresh window opens. The reliability
# runner `docker start`s it again before the next scenario.
docker stop compose-ppz-server-1 >/dev/null

# login+90s is the refresh fire point; +100s guarantees the first
# attempt and at least one 5s retry have both failed.
sleep 100

out=$(ppz_a diagnostics 2>/dev/null)

# Event lines render as: <type> <timestamp> caller=... nc=... reason="..."
# Collect the refresh_error lines once — the reason check below needs
# them anyway. (Never `grep -E … | grep -q` here: the short-circuiting
# grep would SIGPIPE its upstream and flip the verdict, worsening as the
# daemon accumulates events. `matches` in common.sh is the pipeline-safe
# form when you only need the yes/no.)
errs=$(echo "$out" | grep -E '^refresh_error')

if [ -n "$errs" ]; then
  echo "refresh_error=PRESENT"
else
  echo "refresh_error=ABSENT"
fi

# The reason must carry the underlying error, not be empty. Count lines
# whose reason opens with a non-quote char (reason="x… , i.e. non-empty);
# reason="" has a quote immediately after = and is not counted.
withreason=$(grep -cE 'reason="[^"]' <<<"$errs")
if [ "${withreason:-0}" -gt 0 ]; then
  echo "reason=PRESENT"
else
  echo "reason=ABSENT"
fi
