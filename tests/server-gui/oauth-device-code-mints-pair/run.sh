#!/usr/bin/env bash
# Phase 2: POST /oauth/device/code returns a fresh device_code +
# user_code pair. No auth required (this is the CLI initiation step).
. /tests/lib/common.sh

echo "--- mint a device code ---"
body=$(curl_server "/oauth/device/code" -X POST \
    -H "Content-Type: application/json" -s)

echo "device_code present: $(echo "$body" | grep -oE '"device_code":"[^"]+"' | grep -oE '[^"]{32,}' >/dev/null && echo true || echo false)"
echo "user_code XXXX-XXXX: $(echo "$body" | grep -oE '"user_code":"[A-Z0-9]{4}-[A-Z0-9]{4}"' >/dev/null && echo true || echo false)"
echo "verification_uri present: $(echo "$body" | matches -E '"verification_uri":"[^"]+/oauth/device/verify"' && echo true || echo false)"
echo "interval ≥ 1: $(echo "$body" | grep -oE '"interval":[0-9]+' | matches -E '[1-9]' && echo true || echo false)"
echo "expires_in ≥ 60: $(echo "$body" | grep -oE '"expires_in":[0-9]+' | awk -F: '$2 >= 60 {print "true"; exit} END{}' || echo false)"
