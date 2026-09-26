#!/bin/sh
# run-job.sh — invoked by /etc/crontabs/root (see config/scheduler-crontab),
# never by hand. Sources the environment scheduler-entrypoint.sh captured
# at container startup (docker compose's env_file: .env plus its
# environment: block — DATABASE_URL, REDIS_URL, APP_API_KEY, MONNIFY_*, all
# of it), turns this script's one argument into APP_MODE, and execs the
# same static binary the main api/worker containers run. Redirects the
# job's own stdout/stderr to PID 1's (crond's) — busybox crond otherwise
# swallows a job's output rather than logging it, which would make
# `docker compose logs scheduler` show nothing ever ran.
#
# set -e: a job whose exit code cron would otherwise ignore now shows up
# as this script's own non-zero exit in crond's log line for it.
set -e

if [ -z "$1" ]; then
    echo "run-job.sh: missing APP_MODE argument" >&2
    exit 1
fi

. /etc/scheduler.env
export APP_MODE="$1"
exec /payroll-app >>/proc/1/fd/1 2>>/proc/1/fd/2
