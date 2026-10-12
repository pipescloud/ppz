#!/usr/bin/env bash
# `POST /orgs/<id>/keys/<kid>/revoke` soft-revokes an API key. After
# revocation the key MUST be rejected at /auth/exchange — daemons
# that try to log in with it get E_INVALID_API_KEY exactly as if the
# key never existed.
. /tests/lib/common.sh
auth_as_foo

org_id="$(cat /seed/org-alpha.txt)"

# Create a fresh key just for this test so we don't disturb the seeded
# key other tests rely on.
resp="$(curl_server "/orgs/$org_id/keys" -X POST --data-urlencode 'label=revoke-test')"
plaintext="$(printf '%s' "$resp" | grep -oE 'data-new-key="[^"]+"' | sed -E 's/data-new-key="([^"]+)"/\1/')"
key_id="$(printf  '%s' "$resp" | grep -oE 'data-key-id="[^"]+"'  | sed -E 's/data-key-id="([^"]+)"/\1/')"

echo "--- new-key issuance returned both plaintext and id ---"
[[ -n "$plaintext" ]] && echo "plaintext=present" || echo "plaintext=MISSING"
[[ -n "$key_id"    ]] && echo "key_id=present"    || echo "key_id=MISSING"

echo "--- login with the newly-issued key succeeds ---"
ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$plaintext" 2>&1 \
  | grep -oE '^logged in' \
  | head -1

echo "--- revoke returns 200 ---"
curl_server "/orgs/$org_id/keys/$key_id/revoke" -X POST -o /dev/null -w 'http=%{http_code}\n'

echo "--- login with the same (now revoked) key is rejected ---"
ppz_a daemon logout >/dev/null 2>&1 || true
# Capture stderr+stdout, then grep — `set -o pipefail` is on in the
# harness so a piped login's nonzero exit would taint the script's rc;
# we explicitly ignore it.
{ ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$plaintext" 2>&1 || true; } \
  | grep '^error:' \
  | head -1
