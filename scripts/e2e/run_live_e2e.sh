#!/usr/bin/env bash
# run_live_e2e.sh — boots a production-shaped local stack and drives it with
# live_flow_test.py as if real employers, workers, Monnify and attackers were
# using it.
#
# "Production-shaped" means:
#   - the API and worker connect as payroll_app (NOSUPERUSER NOBYPASSRLS), so
#     row-level security applies exactly as in docker-compose; migrations run
#     as the superuser through MIGRATION_DATABASE_URL, the same split
#     docker-compose uses;
#   - a dedicated Redis on its own port, so idempotency keys, OTP codes,
#     rate-limit state and the asynq queue start empty on every run;
#   - the API sits behind a trusted proxy (TRUSTED_PROXIES=127.0.0.1), so each
#     persona gets its own client IP via X-Forwarded-For, like real users on
#     different phones.
#
# What is NOT production: MOCK_MODE=true (Monnify transfers and bank linking
# are simulated, and OTP codes are written to the API log instead of SMS),
# because there is no sandbox credential or SMS provider to test against.
# The script plays Monnify itself by sending HMAC-signed webhooks.
#
# Usage:
#   scripts/e2e/run_live_e2e.sh            # fresh database every run
#   E2E_OUT=/some/dir scripts/e2e/run_live_e2e.sh
#
# Needs: go, python3 (+ requests), psql, redis-server, a local Postgres
# reachable as a superuser (PGHOST/PGPORT/PGUSER/PGPASSWORD, default
# localhost:5432 postgres/postgres).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

PGHOST="${PGHOST:-localhost}"
PGPORT="${PGPORT:-5432}"
PGUSER="${PGUSER:-postgres}"
PGPASSWORD="${PGPASSWORD:-postgres}"
export PGHOST PGPORT PGUSER PGPASSWORD

DB_NAME="${E2E_DB_NAME:-payroll_live}"
APP_DB_PASSWORD="${E2E_APP_DB_PASSWORD:-e2e-app-password}"
API_PORT="${E2E_API_PORT:-28080}"
REDIS_PORT="${E2E_REDIS_PORT:-26379}"
E2E_OUT="${E2E_OUT:-$(mktemp -d -t payroll-e2e-XXXXXX)}"
mkdir -p "$E2E_OUT"

log() { printf '\033[1;34m[e2e]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[e2e] %s\033[0m\n' "$*" >&2; exit 1; }

for bin in go python3 psql redis-server redis-cli; do
  command -v "$bin" >/dev/null || die "missing dependency: $bin"
done
python3 -c 'import requests' 2>/dev/null || die "python3 'requests' package is required (pip install requests)"

PIDS=()
cleanup() {
  # Stop the app first and wait for its graceful shutdown, then Redis — the
  # other way round fills worker.log with connection-refused noise.
  for pid in "${PIDS[@]:-}"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done
  for pid in "${PIDS[@]:-}"; do
    [[ -n "$pid" ]] && wait "$pid" 2>/dev/null || true
  done
  redis-cli -p "$REDIS_PORT" shutdown nosave >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- Database: fresh DB, production-shaped role -------------------------------
log "creating fresh database $DB_NAME"
psql -qAt -v ON_ERROR_STOP=1 -d postgres \
  -c "DROP DATABASE IF EXISTS $DB_NAME WITH (FORCE)" \
  -c "CREATE DATABASE $DB_NAME"

# Mirrors config/postgres-init.sql (which docker runs once per cluster). The
# role is cluster-wide and may already exist; grants are per database.
psql -qAt -v ON_ERROR_STOP=1 -d "$DB_NAME" <<SQL
DO \$\$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'payroll_app') THEN
    CREATE ROLE payroll_app NOSUPERUSER NOBYPASSRLS LOGIN;
  END IF;
END
\$\$;
ALTER ROLE payroll_app WITH NOSUPERUSER NOBYPASSRLS LOGIN PASSWORD '$APP_DB_PASSWORD';
GRANT USAGE ON SCHEMA public TO payroll_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO payroll_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO payroll_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO payroll_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO payroll_app;
-- Operator tooling only (hashing employer passwords during provisioning);
-- the application itself never uses it.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
SQL

# --- Redis: dedicated, empty --------------------------------------------------
log "starting a dedicated redis on :$REDIS_PORT"
redis-server --port "$REDIS_PORT" --save "" --appendonly no --daemonize yes \
  --logfile "$E2E_OUT/redis.log" >/dev/null
for _ in $(seq 1 20); do redis-cli -p "$REDIS_PORT" ping >/dev/null 2>&1 && break; sleep 0.2; done

# --- Build -----------------------------------------------------------------------
log "building the binary"
go build -o "$E2E_OUT/payroll" ./cmd/api

# --- Shared environment for api / worker / scheduler jobs --------------------
rand_b64() { python3 -c 'import os,base64; print(base64.b64encode(os.urandom(32)).decode())'; }
export APP_ENV=development
export MOCK_MODE=true
export APP_API_KEY="e2e-api-key"
export JWT_SECRET="${E2E_JWT_SECRET:-e2e-jwt-secret-$(rand_b64)}"
export ENCRYPTION_KEK="$(rand_b64)"
export ENCRYPTION_HMAC_KEY="$(rand_b64)"
export MONNIFY_SECRET_KEY="${E2E_MONNIFY_SECRET:-e2e-monnify-webhook-secret}"
export DATABASE_URL="postgres://payroll_app:${APP_DB_PASSWORD}@${PGHOST}:${PGPORT}/${DB_NAME}?sslmode=disable"
export MIGRATION_DATABASE_URL="postgres://${PGUSER}:${PGPASSWORD}@${PGHOST}:${PGPORT}/${DB_NAME}?sslmode=disable"
export MIGRATIONS_PATH="file://internal/db/migrations"
export REDIS_URL="localhost:${REDIS_PORT}"
export REDIS_PASSWORD=""
export TRUSTED_PROXIES="127.0.0.1"
export DATA_REGIONS="ng"
export PORT="$API_PORT"

# --- API + worker -------------------------------------------------------------
log "starting API on :$API_PORT (logs: $E2E_OUT/api.log)"
APP_MODE=api "$E2E_OUT/payroll" >"$E2E_OUT/api.log" 2>&1 &
PIDS+=("$!")
for _ in $(seq 1 100); do
  curl -fsS "http://127.0.0.1:${API_PORT}/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "http://127.0.0.1:${API_PORT}/healthz" >/dev/null || die "API did not come up; see $E2E_OUT/api.log"

log "starting worker (logs: $E2E_OUT/worker.log)"
APP_MODE=worker "$E2E_OUT/payroll" >"$E2E_OUT/worker.log" 2>&1 &
PIDS+=("$!")
sleep 1

# --- Drive it -----------------------------------------------------------------
log "running live flow test"
set +e
python3 "$REPO_ROOT/scripts/e2e/live_flow_test.py" \
  --base-url "http://127.0.0.1:${API_PORT}" \
  --api-log "$E2E_OUT/api.log" \
  --operator-dsn "$MIGRATION_DATABASE_URL" \
  --binary "$E2E_OUT/payroll" \
  --out "$E2E_OUT" "$@"
status=$?
set -e

log "artifacts in $E2E_OUT (report.md, report.json, api.log, worker.log)"
exit "$status"
