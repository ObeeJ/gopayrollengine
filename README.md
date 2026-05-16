# go-payroll-engine

A production-grade payroll disbursement engine built in Go, designed to global FinTech engineering standards. Built as a portfolio demonstration of secure, compliant, observable backend systems.

[![CI](https://github.com/obeej/go-payroll-engine/actions/workflows/ci.yml/badge.svg)](https://github.com/obeej/go-payroll-engine/actions)
![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)
![License](https://img.shields.io/badge/license-MIT-green)

---

## What this is

A fully async payroll processing system that handles bulk salary disbursements via Monnify, with production-hardening across security, compliance, and observability layers. Every design decision maps to a real FinTech standard (PCI DSS, NDPR, SOC 2, CBN).

This is not a tutorial project. It is the kind of backend a Series A FinTech would run in production.

---

## Architecture

```
HTTP API (Gin)
    │
    ├── JWT Auth + RBAC
    ├── Multi-tenant scoping (org_id on every query)
    ├── Rate limiting (token bucket, per key)
    ├── Idempotency (Redis hash map, 24h TTL)
    ├── PII encryption (AES-256-GCM, transparent GORM serializer)
    └── Prometheus metrics + structured logging
         │
         ▼
    Asynq Worker (Redis-backed)
         │
         ├── FSM state machine (no illegal status transitions)
         ├── Atomic counter reconciliation (O(1) per webhook)
         └── Monnify bulk transfer API
              │
              ▼
         Webhook Handler
              ├── HMAC-SHA512 signature verification
              ├── Bloom filter deduplication (O(1), ~1% FP rate)
              └── Append-only audit log
```

---

## Engineering highlights

### Security
- **AES-256-GCM envelope encryption** on all PII fields (account numbers, bank codes) — transparent via a custom GORM `EncryptedString` type
- **JWT authentication** with org-scoped claims replacing shared API keys
- **HMAC-SHA512 webhook verification** with constant-time comparison
- **Timing-safe API key comparison** (prevents timing oracle attacks)
- **Non-root Docker** container with distroless final stage
- **Production guards** — process refuses to start if `MOCK_MODE=true` in production, or if `ENCRYPTION_KEK` / `JWT_SECRET` are missing

### Data integrity
- **Finite State Machine** for payroll lifecycle — illegal transitions (e.g. `completed → processing`) are rejected before any DB write
- **Idempotency keys** on all mutating endpoints — a network retry cannot create two payroll batches or pay employees twice
- **Atomic counter reconciliation** — `pending_count` on the Payroll row decrements on each webhook; reconciliation fires exactly once when it hits zero, not on every event
- **Bloom filter** (Redis bitfield, double hashing, 7 hash functions) — catches ~99% of duplicate webhook events before a DB read

### Compliance
- **Versioned SQL migrations** (golang-migrate) — no AutoMigrate in production
- **Immutable audit log** (`audit_events` table, append-only, keyset-paginated)
- **NDPR consent records** — append-only consent trail per employee per consent type
- **BVN verification** via Dojah API at employee creation (CBN KYC requirement)
- **Multi-tenancy** — `organization_id` on every table, `ScopedDB()` helper enforces isolation
- **Data residency middleware** — rejects requests from orgs whose `data_region` doesn't match the deployment region
- **SOC 2 evidence collector** — daily JSON snapshots of audit events, migration version, payroll activity, security checks

### Observability
- **Prometheus metrics** — HTTP request rates/latency, Monnify call success %, payroll processing duration, auth failures, rate limit hits, BVN verification outcomes
- **Pre-built Grafana dashboard** — 11 panels covering all critical FinTech signals
- **Structured JSON logging** (`log/slog`) with PII redaction middleware
- **Request ID injection** — every log line carries a `request_id` for correlation
- **Enhanced `/readyz`** — checks PostgreSQL, Redis, and encryption key presence before accepting traffic

### DSA patterns used
| Pattern | Where | Why |
|---|---|---|
| Hash Map | Worker employee lookup | O(1) per item vs O(N) DB queries (N+1 fix) |
| Token Bucket | Rate limiter | Per-key sustained throttle with burst allowance |
| Bloom Filter | Webhook deduplication | O(1) probabilistic duplicate detection |
| Finite State Machine | Payroll lifecycle | Prevents illegal status transitions |
| Atomic Counter | Webhook reconciliation | O(1) trigger vs O(N) COUNT queries |
| Weighted Sliding Window | Cash flow prediction | Bias toward recent payroll data |
| Append-Only Log | Audit trail | Immutable history for compliance |
| Envelope Encryption | PII at rest | KEK + DEK separation, KMS-ready |

---

## Tech stack

| Layer | Technology |
|---|---|
| Language | Go 1.22+ |
| Web framework | Gin |
| ORM | GORM (PostgreSQL) |
| Background jobs | Asynq (Redis) |
| Migrations | golang-migrate |
| Auth | JWT (golang-jwt/jwt/v5) + bcrypt |
| Metrics | Prometheus + Grafana |
| Payment gateway | Monnify |
| KYC | Dojah (BVN verification) |

---

## Running locally

```bash
# 1. Copy env and fill in values
cp .env.example .env

# 2. Start infrastructure
docker-compose up db redis -d

# 3. Seed the database
APP_MODE=seed go run cmd/api/main.go

# 4. Start the API
APP_MODE=api go run cmd/api/main.go

# 5. Start the worker (separate terminal)
APP_MODE=worker go run cmd/api/main.go
```

### Full observability stack (optional)
```bash
docker-compose up  # starts API + worker + Prometheus + Grafana + exporters
# Grafana: http://localhost:3000 (admin / changeme)
# Prometheus: http://localhost:9090
# Metrics: http://localhost:8080/metrics
```

---

## API endpoints

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/api/v1/auth/login` | Public | Issue JWT |
| POST | `/api/v1/auth/refresh` | JWT | Refresh token |
| POST | `/api/v1/employees/` | JWT + Idempotency | Create employee (BVN verified) |
| GET | `/api/v1/employees/` | JWT | List employees (paginated) |
| POST | `/api/v1/payrolls/` | JWT + Idempotency | Initiate payroll batch |
| GET | `/api/v1/payrolls/:id` | JWT | Get batch status + items |
| GET | `/api/v1/analytics/predictive` | JWT | Cash flow forecast + risk level |
| POST | `/api/v1/consent/` | JWT | Record NDPR consent |
| GET | `/api/v1/consent/:employee_id` | JWT | Consent history |
| GET | `/api/v1/compliance/report` | JWT (role=compliance) | SOC 2 / CBN evidence bundle |
| POST | `/api/v1/webhooks/monnify` | HMAC | Disbursement reconciliation |
| GET | `/healthz` | Public | Liveness probe |
| GET | `/readyz` | Public | Readiness probe |
| GET | `/metrics` | Public | Prometheus scrape |

---

## Environment variables

See `.env.example` for the full list with descriptions. Key variables:

```env
JWT_SECRET=          # Token signing key — rotate to invalidate all sessions
ENCRYPTION_KEK=      # Base64 32-byte key for AES-256-GCM PII encryption
APP_API_KEY=         # Machine-to-machine auth key
MOCK_MODE=true       # Bypasses Monnify calls for local testing
DATA_REGIONS=ng      # Comma-separated allowed regions for data residency
```

---

## Testing

```bash
go test ./... -race -count=1
```

---

## License

MIT — use it, fork it, learn from it.
