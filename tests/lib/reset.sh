#!/usr/bin/env bash
# Reset shared state between scenarios + record what we did to
# /tmp/ppz-diag.log so post-mortems on transient failures have
# something to chew on.
#
# 1. Truncate `sources` and remove any orgs/api_keys created by GUI tests
#    (the seeded fixtures alpha + beta + their three keys are kept).
# 2. Wipe daemon-side credentials + current files. The daemon polls these
#    every 200ms and reloads automatically, so no signal is needed (and
#    cross-container kill wouldn't reach it anyway).
# 3. Drop JetStream streams across every per-org account via the server's
#    admin/wipe endpoint. Phase 3.5 introduced per-org accounts, which
#    means a single nats CLI session can no longer see across them —
#    ppz-server is the only thing that holds connections to every
#    account, so cleanup belongs there.
#
# Idempotent and safe to run before every scenario.
#
# Reads env:
#   SCENARIO  — set by tests/run.sh; appears in /tmp/ppz-diag.log
#               so we know which scenario each diag entry belongs to.
#
# Errors are NOT silenced — silent reset failures led to ghost streams
# bleeding between scenarios with no signal in the test output. We'd
# rather crash loud and fix the cause.

set -u

DIAG=${PPZ_DIAG_LOG:-/tmp/ppz-diag.log}
SCENARIO=${SCENARIO:-?}
START_TS=$(date -u +%s.%N 2>/dev/null || date -u +%s)
echo "── reset.sh @ $(date -u +%FT%TZ)  scenario=$SCENARIO" >> "$DIAG"

if command -v psql >/dev/null 2>&1; then
  PGPASSWORD=ppz psql -h postgres -U postgres -d ppz -v ON_ERROR_STOP=0 >/dev/null 2>&1 <<'SQL' || true
TRUNCATE TABLE pipes;
TRUNCATE TABLE sources CASCADE;
-- Web chat read cursors are per-user, per-window and scenario-local; a
-- leaked cursor makes a later scenario's "unread" count start non-zero.
-- Tolerated if the 0005 migration hasn't landed (ON_ERROR_STOP=0).
TRUNCATE TABLE chat_read_cursors;
-- Scheduled sends (docs/specs/schedule.md): schedules are scenario-
-- local, and a leaked `--every 1s` row would keep the server firing
-- into every later scenario. Errors are tolerated (ON_ERROR_STOP=0)
-- until the 0004_schedules migration lands.
TRUNCATE TABLE schedules;
-- Phase 4: invites are scenario-local; clear them so a prior run's
-- declined/revoked rows don't bleed into the next scenario's count.
DELETE FROM invites;
DELETE FROM api_keys WHERE label NOT IN ('alpha-primary','alpha-secondary','beta-primary');
-- Un-revoke seeded keys so each scenario starts with all three usable.
UPDATE api_keys SET revoked_at = NULL WHERE revoked_at IS NOT NULL;
DELETE FROM accounts WHERE name NOT IN ('alpha','beta');
-- Users v1: drop any non-seeded users (alice/bob etc. created by
-- tests). The 'unauthenticated' placeholder MUST stay so alpha/beta
-- retain a valid owner_user_id FK target. foo + bar are seeded
-- test fixtures (see internal/seed/seed.go) — keep them so member-
-- management tests can rely on stable user ids.
DELETE FROM users WHERE username NOT IN ('unauthenticated', 'foo', 'bar');
-- Clear the per-user "last selected org" preference. It's scenario-local
-- (set when a user authorizes a CLI session into a specific org via the
-- device flow); without this reset a prior scenario's choice bleeds into
-- the next scenario's default-org assertions.
UPDATE users SET last_selected_account_id = NULL;
-- Reset memberships to the seeded baseline. Auth V2 widened this so
-- bar is also a member of alpha (used by owner-only-gate tests).
DELETE FROM account_members;
INSERT INTO account_members (account_id, user_id)
  SELECT o.id, u.id
    FROM accounts o, users u
   WHERE (o.name = 'alpha' AND u.username = 'foo')
      OR (o.name = 'alpha' AND u.username = 'bar')
      OR (o.name = 'beta'  AND u.username = 'bar')
ON CONFLICT DO NOTHING;
-- Reset alpha's owner to foo so owner-only-gate tests have a stable
-- owner across scenarios (without this, a previous test could
-- transfer ownership and leave us in an inconsistent state).
UPDATE accounts
   SET owner_user_id = (SELECT id FROM users WHERE username = 'foo')
 WHERE name = 'alpha';
SQL
fi

# JetStream cleanup via ppz-server's admin endpoint (Phase 3.5).
# Server iterates every provisioned per-org account and deletes
# `source_*`/`pipe_*` streams in each. Idempotent.
SERVER_URL="${PPZ_SERVER_URL:-http://ppz-server:8080}"
status=$(curl -sS -o /dev/null -w "%{http_code}" -X POST "$SERVER_URL/api/v1/admin/wipe" 2>/dev/null || echo "0")
if [ "$status" != "200" ]; then
  echo "  ERROR: admin/wipe returned $status" >> "$DIAG"
  echo "reset.sh: admin/wipe returned $status (server-side stream cleanup failed)" >&2
  exit 1
fi
echo "  admin/wipe ok (status=$status)" >> "$DIAG"

reset_daemon_home() {
  local home="$1"
  [[ -d "$home" ]] || return 0
  # The daemon's file watcher (50ms tick + stat-based diff) reliably
  # catches mtime/size changes but — observed in this compose's
  # named-volume mount — does NOT reliably observe pure unlink. The
  # symptom was a flake on login/login-bad-key-rejected: the prior
  # scenario logged in, reset rm'd credentials, the daemon kept
  # reporting logged-in for several seconds, the test asserted
  # "not logged in" and failed.
  #
  # Workaround: truncate credentials in place (size 1035 → 0, fresh
  # mtime). The watcher sees the size change, calls LoadFromDisk, the
  # empty file fails json.Unmarshal, creds is left nil. The next
  # `ppz daemon login` overwrites the empty file via the same path
  # as a fresh write. We don't unlink — that's the operation the
  # watcher misses.
  if [[ -f "$home/credentials" ]]; then
    : > "$home/credentials" 2>/dev/null || true
  fi
  # `current` is the legacy plain-text file (pre-per-session); `current.json`
  # is the per-session map. `namespace.json` is Phase 1.5's per-session
  # manifold map. Truncate (not unlink) for the same reason.
  for f in "$home/current" "$home/current.json" "$home/namespace.json"; do
    if [[ -f "$f" ]]; then
      : > "$f" 2>/dev/null || true
    fi
  done
  rm -f "$home/current.json.tmp" "$home/namespace.json.tmp" 2>/dev/null || true
  rm -rf "$home/cursors" 2>/dev/null || true
  # Per-session pipe-subscription lists (ppz subs). Same per-session state
  # as cursors — wipe between scenarios so subs added in one don't bleed
  # into the next (e.g. a `mysh` session reused across scenarios).
  rm -rf "$home/subs" 2>/dev/null || true
}
reset_daemon_home "${PPZ_DAEMON_A_HOME:-/tmp/a}"
reset_daemon_home "${PPZ_DAEMON_B_HOME:-/tmp/b}"

# Active-wait for the daemon's 50ms file poller to pick up the deletions.
# Replaced a blanket `sleep 0.3` — across 157 scenarios that was ~47s of
# pure idle wait. Polls `ppz status` every 50ms; typical case <100ms.
# Capped at ~1s — combined with the truncate-before-rm in reset_daemon_home
# the watcher reliably observes the change in <100ms; this is a safety
# net for genuinely slow ticks.
wait_daemon_logged_out() {
  local sock="$1"
  [[ -S "$sock" ]] || return 0
  local i out
  for ((i = 0; i < 20; i++)); do
    out=$(PPZ_IPC_SOCKET="$sock" ppz status 2>/dev/null || true)
    case "$out" in
      *"not logged in"*|*"not running"*) return 0 ;;
    esac
    sleep 0.05
  done
  echo "  WARN: daemon at $sock still reports logged-in after 1s wait" >> "$DIAG"
}
wait_daemon_logged_out "${PPZ_DAEMON_A_HOME:-/tmp/a}/daemon.sock"
wait_daemon_logged_out "${PPZ_DAEMON_B_HOME:-/tmp/b}/daemon.sock"

END_TS=$(date -u +%s.%N 2>/dev/null || date -u +%s)
elapsed=$(awk -v a="$END_TS" -v b="$START_TS" 'BEGIN{printf "%.3f", a-b}' 2>/dev/null || echo "?")
echo "  ok elapsed=${elapsed}s" >> "$DIAG"

exit 0
