#!/usr/bin/env bash
# Negative case: an attacker hitting NATS on :4222 with no credentials
# at all (mirrors a pre-Phase-3 daemon, or any random internet client)
# must be rejected. Without this, opening 4222 to 0.0.0.0/0 is unsafe.
. /tests/lib/common.sh

echo "--- raw nats.Connect with no creds ---"
out=$(nats --server="nats://ppz-server:4222" \
    pub "anything.foo.broadcast" "no-auth attempt" 2>&1 || true)
if echo "$out" | matches -iE 'authorization|unauthorized|permissions'; then
  echo "rejected=true"
else
  echo "rejected=false"
  echo "  unexpected output: $out"
fi
