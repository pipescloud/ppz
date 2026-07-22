#!/usr/bin/env bash
# Phase 1.5.2: `ppz source destroy <handle>` finds the source regardless
# of which manifold it lives at. Pre-1.5.2 untested — the destroy
# resolver might be assuming root-manifold scope. Locks in cross-
# manifold lookup.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a unset handle >/dev/null
ppz_a set namespace pixel >/dev/null
ppz_a source create boris >/dev/null
ppz_a unset namespace >/dev/null

echo "--- before destroy ---"
ppz_a ls | ls_normalize | awk '$1 ~ /boris/ {print $1}' | sort

# Plain destroy without re-setting the namespace. Resolver must find
# the source at its actual manifold (pixel.boris), not root.
ppz_a source destroy boris 2>&1 | head -1

echo "--- after destroy ---"
# `matches`, not `grep -q`: a short-circuiting grep would SIGPIPE the
# upstream sort and fire the `|| echo` even when boris rows exist.
# See matches in common.sh.
ppz_a ls | ls_normalize | awk '$1 ~ /boris/ {print $1}' | sort | matches . || echo "no boris rows"
