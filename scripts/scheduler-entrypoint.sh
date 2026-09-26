#!/bin/sh
# scheduler-entrypoint.sh — Dockerfile.scheduler's CMD. Dumps this
# container's own environment (docker compose's env_file: .env plus its
# environment: block) to a file, then starts crond.
#
# This exists so run-job.sh does not have to depend on whether busybox
# crond forwards its own process environment unmodified to the jobs it
# spawns — it does, in the version this image pins, but explicitly
# sourcing a file written by the one process (this script) that is
# unambiguously running with docker compose's env intact removes the need
# to rely on that behavior at all, in this or any future base image.
#
# Split each line on the FIRST '=' with parameter expansion
# (${line%%=*}/${line#*=}), not `IFS='=' read key value`: read's
# multi-field reassembly silently drops a *trailing* delimiter with
# nothing after it, which truncates exactly the last character of any
# value ending in '=' — proven empirically while writing this, not assumed
# — and two of this app's own env vars are base64 secrets that pad with
# trailing '=' (ENCRYPTION_KEK, ENCRYPTION_HMAC_KEY). Parameter expansion
# has no such gotcha; it is a pure substring operation.
set -eu

: >/etc/scheduler.env
env | while IFS= read -r line; do
    key=${line%%=*}
    [ -n "$key" ] || continue
    value=${line#*=}
    escaped=$(printf '%s' "$value" | sed "s/'/'\\\\''/g")
    printf "export %s='%s'\n" "$key" "$escaped" >>/etc/scheduler.env
done

exec crond -f -l 8
