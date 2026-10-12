#!/usr/bin/env bash
# `ppz pipe purge` empties a pipe's buffer but keeps the pipe: collared,
# uncollared (bare NAME at the current namespace) and auto-provisioned
# pipes alike. Sequence numbers survive, so a reader's cursor stays valid:
# after a purge it sees only what's published next.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create desk >/dev/null
ppz_a pipe create desk.log >/dev/null
for i in 1 2 3; do ppz_a send desk.log "log-$i" >/dev/null; done
ppz_a read desk.log >/dev/null   # advance this session's cursor past log-3

echo "--- collared ---"
ppz_a pipe purge desk.log
echo "buffered after purge: $(ppz_a reread desk.log | grep -c 'log-')"
ppz_a send desk.log "log-4" >/dev/null
echo "next read: $(ppz_a read desk.log | grep -oE 'log-[0-9]+' | tr '\n' ' ')"

echo "--- auto-provisioned ---"
ppz_a send desk.inbox "hello" >/dev/null
ppz_a pipe purge desk.inbox

echo "--- uncollared at the current namespace ---"
ppz_a set namespace team-a >/dev/null
ppz_a pipe create general >/dev/null
ppz_a send general "hi" >/dev/null
ppz_a pipe purge general
echo "pipe still listed: $(ppz_a ls | ls_normalize | cut -d' ' -f1 | grep -c '^team-a.general$')"

echo "--- unknown pipe ---"
ppz_a pipe purge desk.nope 2>&1 || true
