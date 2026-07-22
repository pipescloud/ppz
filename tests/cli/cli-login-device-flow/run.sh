#!/usr/bin/env bash
# Phase 2: `ppz login URL` (no -apikey) opens our device flow.
#
# Expected CLI shape:
#   $ ppz login http://ppz-server:8080
#   To authorize this device, open:
#     http://ppz-server:8080/oauth/device/verify?user_code=ABCD-1234
#   Or visit /oauth/device/verify and enter: ABCD-1234
#   waiting...
#   ✓ logged in as foo
#
# This test runs the CLI in the background, parses the user_code from
# its stdout, drives the browser-side approval as foo (via /dev/login
# + POST /oauth/device/verify), then waits for the CLI to exit clean.
. /tests/lib/common.sh

# 1. Launch ppz login in the background, capture its stdout.
LOG=$(mktemp)
ppz_a login "$PPZ_SERVER_URL" > "$LOG" 2>&1 &
CLI_PID=$!

# 2. Poll the log for the printed user_code (CLI prints it shortly
#    after kicking off /oauth/device/code).
USER_CODE=""
for i in $(seq 1 30); do
  USER_CODE=$(grep -oE '[A-Z0-9]{4}-[A-Z0-9]{4}' "$LOG" | head -1)
  [[ -n "$USER_CODE" ]] && break
  sleep 0.5
done
echo "user_code_extracted: $([[ -n "$USER_CODE" ]] && echo true || echo false)"

# 3. As foo, approve the user_code via the browser endpoint.
COOKIE_JAR=$(mktemp)
trap "rm -f $COOKIE_JAR $LOG" EXIT
curl_server "/dev/login?user=foo" -X POST -c "$COOKIE_JAR" -o /dev/null -s
curl_server "/oauth/device/verify" -X POST -b "$COOKIE_JAR" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "user_code=$USER_CODE" \
    --max-redirs 0 -o /dev/null -s

# 4. Wait for the CLI to detect approval + exit.
wait "$CLI_PID"
CLI_RC=$?
echo "cli_exit_code: $CLI_RC"

# 5. Daemon should now be authed.
echo "--- daemon status ---"
ppz_a status | matches -E 'logged in' && echo "login=ok" || echo "login=fail"

# 6. Daemon can list pipes (proves the bearer works).
echo ""
echo "--- ls works against the API ---"
ppz_a ls >/dev/null && echo "ls=ok" || echo "ls=fail"
