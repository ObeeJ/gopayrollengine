#!/usr/bin/env bash
# Codebase invariants — fast, dependency-free guard rails enforced on every
# CI run. Each check below corresponds to a real production incident class.
set -uo pipefail

PASS=0
FAIL=0
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

check() {
	local name="$1"; shift
	if "$@" >/dev/null 2>&1; then
		printf "  \033[32mPASS\033[0m  %s\n" "$name"
		PASS=$((PASS+1))
	else
		printf "  \033[31mFAIL\033[0m  %s\n" "$name"
		FAIL=$((FAIL+1))
	fi
}

echo "== Codebase invariants =="

# 1. Compilation must succeed — broken main blocks every other check.
check "go build ./..." go build ./...

# 2. go vet must be clean — catches printf format bugs, lock copies, etc.
check "go vet ./..." go vet ./...

# 3. Amounts must use money.Kobo or int64 — never float in DB-bound models.
#    Float drift in payroll caused the entire money package to exist.
check "no float64 for monetary fields in workers" \
	bash -c '! grep -RnE "Amount[[:space:]]+float" internal/workers/ 2>/dev/null'

# 4. Required env vars must be referenced somewhere in the code.
for var in JWT_SECRET ENCRYPTION_KEK MONNIFY_SECRET_KEY DATABASE_URL REDIS_URL; do
	check "env var $var is referenced" grep -Rq "\"$var\"" internal/ cmd/
done

# 5. Webhook handler must use constant-time HMAC comparison (timing-attack safe).
check "webhook HMAC uses constant-time compare" \
	grep -q "hmac.Equal" internal/api/handlers/webhook_handler.go

# 6. JWT middleware must reject non-HMAC algorithms (alg=none confusion attack).
check "JWT middleware rejects non-HMAC algorithms" \
	grep -q "SigningMethodHMAC" internal/api/middleware/auth.go

# 7. Encryption must be AES-GCM (authenticated), never AES-CBC/ECB.
check "encryption uses AES-GCM" grep -q "cipher.NewGCM" internal/models/encryption.go
check "no AES-CBC in models" bash -c '! grep -Rn "cipher.NewCBC" internal/models/ 2>/dev/null'

# 8. Audit log must be append-only — no Update/Delete on AuditEvent.
check "AuditEvent has no Update calls" \
	bash -c '! grep -Rn "Model(&AuditEvent\|Update(\"audit" internal/ 2>/dev/null'

# 9. The FSM map must list exactly the four canonical statuses.
check "FSM contains all four statuses" \
	bash -c 'grep -q "PayrollPending" internal/models/models.go && grep -q "PayrollProcessing" internal/models/models.go && grep -q "PayrollCompleted" internal/models/models.go && grep -q "PayrollFailed" internal/models/models.go'

# 10. Completed is terminal — must map to an empty transition list.
check "PayrollCompleted is terminal in FSM" \
	grep -q "PayrollCompleted:.*{}" internal/models/models.go

echo
echo "== $PASS passed, $FAIL failed =="
[ "$FAIL" -eq 0 ]
