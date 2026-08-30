#!/usr/bin/env bash
# `ppz ls -l` is the read side of `ppz pipe set`. Before it, retention
# could be changed but never inspected: the caps were echoed once in the
# reply line at the moment of the change and then invisible, so "what is
# this pipe's cap right now?" had no answer short of re-running the
# mutation.
#
# Modelled on `ls -l`: same rows, extra detail columns, opt-in.
. /tests/lib/common.sh

ppz_a daemon login "$PPZ_SERVER_URL" -apikey "$(key_alpha)" >/dev/null
ppz_a source create chat >/dev/null
ppz_a pipe create chat.archive >/dev/null
ppz_a pipe set chat.archive --ttl=1h --max-msgs=500 --max-bytes=1048576 >/dev/null

echo "--- short form is unchanged (no retention columns) ---"
ppz_a ls | head -1 | tr -s ' '

echo "--- long form ---"
ppz_a ls -l | head -1 | tr -s ' '
ppz_a ls -l | ls_normalize | awk '$1 == "chat.archive" {print "ttl="$4" msgs="$5" bytes="$6}'

echo "--- --long is the same flag ---"
ppz_a ls --long | ls_normalize | awk '$1 == "chat.archive" {print "ttl="$4" msgs="$5" bytes="$6}'
