#!/usr/bin/env bash
# ACL Phase 2: grant, then see the row — with its provenance.
#
# alice.stdout is owner-only by default. The roster must show BOTH the
# derived access (foo, who owns the handle and the org) and the stored
# grant (bar, and by whom). A view built on acl_grants alone would show
# one row and imply nobody else can reach it.
#
# Note foo owns the handle: `alice` is a HANDLE, not a principal. The
# key's principal created it, so the handle owner is foo.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create alice >/dev/null

echo "--- before the grant ---"
ppz_a pipe acl ls alice.stdout --json | jq -c 'sort_by(.principal) | [.[] | {principal, read, via}]'

echo "--- grant bar read ---"
ppz_a pipe acl grant alice.stdout bar read

echo "--- after the grant ---"
ppz_a pipe acl ls alice.stdout --json | jq -c 'sort_by(.principal) | [.[] | {principal, read, via}]'

echo "--- revoke is idempotent ---"
ppz_a pipe acl revoke alice.stdout bar read
ppz_a pipe acl revoke alice.stdout bar read
