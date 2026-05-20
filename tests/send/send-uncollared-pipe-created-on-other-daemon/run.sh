#!/usr/bin/env bash
# Regression: `ppz send LEAF` for an uncollared pipe created on a
# *different* daemon must succeed. Both daemons are in the same org
# (alpha-primary on A, alpha-secondary on B) so the pipe is visible
# in B's `ppz ls` view, but the send path fails with "pipe not
# found" — the lookup that resolves bare-name → uncollared stream
# only consults state from the local daemon's own creates.
#
# Companion to send-uncollared-pipe (single-daemon happy path):
# that scenario passes; this one fails until the bug is fixed.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)"  >/dev/null
ppz_b daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha2)" >/dev/null

# Both daemons must take the bare-name (uncollared) path on send, so
# neither side may have a current handle that would auto-collar the
# lookup. Namespaces cleared for the same reason.
ppz_a unset handle    >/dev/null 2>&1
ppz_a unset namespace >/dev/null 2>&1
ppz_b unset handle    >/dev/null 2>&1
ppz_b unset namespace >/dev/null 2>&1

# A creates the uncollared pipe; B does NOT create anything and does
# NOT list / refresh first. The send path itself has to resolve the
# bare name → uncollared stream. Stream exists server-side with zero
# messages.
ppz_a pipe create room >/dev/null

err=$(mktemp)
ppz_b send room "remote payload" 2>"$err"
echo "send-exit=$?"
grep -oE '^sent id=[a-f0-9]{8} to=[^ ]+ bytes=[0-9]+$' "$err" | head -1 \
  | sed -E 's/id=[a-f0-9]{8}/id=ID8/; s/bytes=[0-9]+/bytes=N/'

# Read the payload back from A to prove the publish reached the
# shared uncollared stream.
ppz_a reread room -l 1 --bare
rm -f "$err"
