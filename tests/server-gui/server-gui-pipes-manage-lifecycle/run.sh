#!/usr/bin/env bash
# The Pipes tab manages pipes and sources: one create form for a source,
# a collared pipe (attached to a source) or an uncollared pipe, and
# per-row deletes. Changes go through the same ops as the CLI, so the CLI
# sees them, the same validation applies, and the audit trail records
# them as "via web". Auto-provisioned pipes can't be deleted one by one —
# the source can.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
auth_as_foo
post() {
  local path="$1"; shift
  curl_server "/orgs/alpha$path" -X POST "$@" -o /tmp/gui-body -w '%{http_code}'
}
listed() {
  ppz_a ls | ls_normalize | cut -d' ' -f1 | grep -E '^(desk\.|team-a\.general$)' | sort || echo "(none)"
}

echo "create source: $(post /pipes -d kind=source -d name=desk)"
echo "create collared pipe: $(post /pipes -d kind=pipe -d source=desk -d name=notes)"
echo "create uncollared pipe: $(post /pipes -d kind=pipe -d namespace=team-a -d name=general)"

echo "--- the CLI sees them ---"
listed

echo "--- the table offers deletes (none on the auto-provisioned inbox) ---"
curl_server "/orgs/alpha/pipes" | grep -oE 'data-delete-(pipe|source)="[^"]+"' | sort

echo "--- validation and conflicts match the CLI ---"
echo "bad name: $(post /pipes -d kind=pipe -d name=Bad_Name) $(cat /tmp/gui-body)"
echo "duplicate source: $(post /pipes -d kind=source -d name=desk)"
echo "unknown source: $(post /pipes -d kind=pipe -d source=nope -d name=x)"
echo "auto pipe delete: $(post /pipes/delete -d source=desk -d name=inbox) $(cat /tmp/gui-body)"

echo "--- deletes ---"
echo "delete collared pipe: $(post /pipes/delete -d source=desk -d name=notes)"
echo "delete uncollared pipe: $(post /pipes/delete -d namespace=team-a -d name=general)"
echo "delete source: $(post /sources/delete -d source=desk)"
listed

echo "--- audit trail, newest first ---"
curl_server "/orgs/alpha/audit" | tr '\n' ' ' \
  | grep -oE 'data-audit-action="[^"]+"[[:space:]]+data-audit-target="[^"]+"[[:space:]]+data-audit-actor="[^"]+"[[:space:]]+data-audit-via="[^"]+"' \
  | sed -E 's/data-audit-[a-z]+="([^"]+)"/\1/g; s/[[:space:]]+/ /g'
