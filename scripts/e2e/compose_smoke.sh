#!/usr/bin/env bash
# compose_smoke.sh — brings the docker-compose stack up exactly as deployed
# (distroless images, TLS proxy, payroll_app role, migrations on startup) and
# checks the parts a unit test cannot: that the proxy, the secrets and the
# scrape credential are wired correctly.
#
# Uses APP_ENV=development with MOCK_MODE=true, so this proves the plumbing,
# not Monnify, SMS or a bank-link provider (none have credentials in CI).
#
# Usage: scripts/e2e/compose_smoke.sh      (needs docker compose, curl, jq, openssl)
# Leaves no state behind: it writes a throwaway .env and removes volumes on exit.

set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
check() { local name=$1; shift; if "$@" >/dev/null 2>&1; then ok "$name"; else bad "$name"; fi; }
status_is() { [[ "$(curl -ksS -o /dev/null -w '%{http_code}' "${@:2}")" == "$1" ]]; }

for bin in docker curl jq openssl python3; do command -v "$bin" >/dev/null || { echo "missing: $bin"; exit 2; }; done
[[ -e .env ]] && { echo "refusing to overwrite an existing .env"; exit 2; }

cleanup() {
  local rc=$?
  if (( FAIL > 0 || rc != 0 )); then
    echo "---- compose logs (tail) ----"; docker compose logs --no-color --tail=80 2>&1 || true
  fi
  docker compose down -v --remove-orphans >/dev/null 2>&1 || true
  rm -f .env
}
trap cleanup EXIT

rand() { openssl rand -hex 16; }
cat > .env <<ENV
APP_ENV=development
MOCK_MODE=true
APP_API_KEY=$(rand)
JWT_SECRET=$(rand)$(rand)
ENCRYPTION_KEK=$(openssl rand -base64 32)
ENCRYPTION_HMAC_KEY=$(openssl rand -base64 32)
MONNIFY_SECRET_KEY=$(rand)
POSTGRES_PASSWORD=$(rand)
POSTGRES_APP_PASSWORD=$(rand)
REDIS_PASSWORD=$(rand)
GRAFANA_PASSWORD=$(rand)
METRICS_TOKEN=$(rand)
DATA_REGIONS=ng
ENV
METRICS_TOKEN="$(grep '^METRICS_TOKEN=' .env | cut -d= -f2)"

echo "== building and starting the stack"
docker compose up -d --build

echo "== waiting for the API behind the TLS proxy"
up=0
for _ in $(seq 1 90); do
  if status_is 200 https://localhost/healthz; then up=1; break; fi
  sleep 2
done
(( up )) || { bad "API did not become healthy behind https://localhost"; exit 1; }

echo "== proxy and exposure"
check "healthz over TLS"                         status_is 200 https://localhost/healthz
check "readyz over TLS (db, redis, encryption)"  status_is 200 https://localhost/readyz
check "plain HTTP is redirected to HTTPS"        status_is 308 http://localhost/healthz
check "/metrics is not forwarded by the proxy"   status_is 404 https://localhost/metrics
check "the API port is not published on the host" bash -c '! curl -s --max-time 3 http://127.0.0.1:28080/healthz'
check "HSTS header is sent"                      bash -c "curl -ksSI https://localhost/healthz | grep -qi '^strict-transport-security'"

echo "== onboarding and login through the proxy"
EMAIL="smoke-$(rand | cut -c1-8)@example.com"
OUT="$(docker compose run --rm -T -e APP_MODE=create-org -e ORG_NAME='Smoke Ltd' -e "ADMIN_EMAIL=$EMAIL" api 2>&1 || true)"
TEMP="$(sed -n 's/^temporary password (shown once): //p' <<<"$OUT" | tail -1)"
[[ -n "$TEMP" ]] && ok "create-org prints a one-time password" || { bad "create-org produced no password: $OUT"; exit 1; }

login() { curl -ksS https://localhost/api/v1/auth/login -H 'Content-Type: application/json' \
            -d "$(jq -nc --arg e "$EMAIL" --arg p "$1" '{email:$e,password:$p}')"; }
TOKEN="$(login "$TEMP" | jq -r .token)"
[[ -n "$TOKEN" && "$TOKEN" != null ]] && ok "admin logs in with the temporary password" || bad "login failed"
check "a temporary-password session is locked" \
  status_is 403 https://localhost/api/v1/users/ -H "Authorization: Bearer $TOKEN"
NEWPW="Smoke-pass-$(rand)"
TOKEN2="$(curl -ksS https://localhost/api/v1/auth/password -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg c "$TEMP" --arg n "$NEWPW" '{current_password:$c,new_password:$n}')" | jq -r .token)"
check "after changing the password the admin can list users" \
  status_is 200 https://localhost/api/v1/users/ -H "Authorization: Bearer $TOKEN2"

echo "== observability"
scrape_up=0
for _ in $(seq 1 45); do
  state="$(curl -sS http://127.0.0.1:9090/api/v1/targets 2>/dev/null \
    | jq -r '.data.activeTargets[]? | select(.labels.job=="payroll-api") | .health' | head -1 || true)"
  [[ "$state" == "up" ]] && { scrape_up=1; break; }
  sleep 2
done
(( scrape_up )) && ok "Prometheus scrapes /metrics with the bearer token (target is up)" \
                || bad "Prometheus target payroll-api is not up (state: ${state:-none})"
check "/metrics is closed to an anonymous caller on the internal network" \
  bash -c "docker compose exec -T prometheus wget -q -O /dev/null --server-response http://api:8080/metrics 2>&1 | grep -q '401'"

echo
echo "compose smoke: $PASS passed, $FAIL failed"
(( FAIL == 0 ))
