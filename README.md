<div align="center">

<br/>

<img src="https://capsule-render.vercel.app/api?type=waving&color=0:0f0c29,50:302b63,100:24243e&height=200&section=header&text=go-payroll-engine&fontSize=52&fontColor=ffffff&fontAlignY=38&desc=Production-grade%20FinTech%20Payroll%20Disbursement%20Engine&descAlignY=58&descSize=18&descColor=a78bfa" width="100%"/>

<br/>

[![CI](https://github.com/obeej/go-payroll-engine/actions/workflows/ci.yml/badge.svg)](https://github.com/obeej/go-payroll-engine/actions)
![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat-square&logo=go&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-15-4169E1?style=flat-square&logo=postgresql&logoColor=white)
![Redis](https://img.shields.io/badge/Redis-7-DC382D?style=flat-square&logo=redis&logoColor=white)
![License](https://img.shields.io/badge/License-MIT-a78bfa?style=flat-square)
![Status](https://img.shields.io/badge/Status-Production--Ready-22c55e?style=flat-square)

<br/>

> **Not a tutorial project.**
> The kind of backend a Series A FinTech runs in production —
> built to PCI DSS, NDPR, SOC 2, and CBN standards from day one.

<br/>

</div>

---

<div align="center">

## What This Is

</div>

A fully async payroll disbursement engine that handles bulk salary transfers via **Monnify**, hardened across every layer a global FinTech regulator will examine — security, compliance, observability, and data integrity.

Every design decision in this codebase maps to a real standard. Nothing is bolted on after the fact.

---

<div align="center">

## Architecture

</div>

```
┌─────────────────────────────────────────────────────────────┐
│                        HTTP API  (Gin)                       │
│                                                              │
│  JWT Auth + RBAC  →  Multi-tenant Scoping  →  Rate Limit    │
│  Idempotency Keys  →  PII Encryption  →  Prometheus Metrics  │
└──────────────────────────────┬──────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────┐
│                   Asynq Worker  (Redis)                      │
│                                                              │
│   FSM State Machine  →  N+1 Fix (Hash Map)  →  Monnify API  │
└──────────────────────────────┬──────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────┐
│                    Webhook Handler                           │
│                                                              │
│  HMAC-SHA512 Verify  →  Bloom Filter  →  Atomic Counter     │
│  FSM Transition  →  Audit Log  →  Reconcile                  │
└─────────────────────────────────────────────────────────────┘
```

---

<div align="center">

## Security Layer

</div>

<table>
<tr>
<td width="50%">

**Authentication & Identity**
- JWT with org-scoped claims (multi-tenant)
- RBAC — `RequireRole("compliance")` gate
- Timing-safe API key comparison (`hmac.Equal`)
- bcrypt cost-12 password hashing

</td>
<td width="50%">

**Data Protection**
- AES-256-GCM envelope encryption on all PII
- Transparent `EncryptedString` GORM type
- HMAC-SHA512 webhook signature verification
- PII redaction in all structured logs

</td>
</tr>
<tr>
<td width="50%">

**Transport & Runtime**
- OWASP security headers on every response
- 1MB request body size limit
- Non-root Docker container
- Production startup guards (refuses to boot without KEK / JWT secret)

</td>
<td width="50%">

**Abuse Prevention**
- Token bucket rate limiting per API key / IP
- Idempotency keys — network retry does not equal double payment
- Bloom filter webhook deduplication (~1% FP, O(1))
- Constant-time comparisons throughout

</td>
</tr>
</table>

---

<div align="center">

## DSA Patterns — Every One Justified

</div>

| Pattern | Location | Why It's Here |
|:---|:---|:---|
| Hash Map | `payroll_worker.go` | O(1) employee lookup per item — kills the N+1 query |
| Token Bucket | `middleware/ratelimit.go` | Per-key sustained throttle with burst allowance |
| Bloom Filter | `middleware/bloom.go` | O(1) probabilistic duplicate detection before any DB read |
| Finite State Machine | `models/models.go` | Illegal transitions (e.g. `completed→processing`) rejected before DB write |
| Atomic Counter | `models/models.go` + webhook | `pending_count` decrements per webhook — reconciliation fires exactly once |
| Weighted Sliding Window | `analytics_service.go` | Biases cash flow forecast toward recent payroll data |
| Append-Only Log | `audit_events` table | Immutable history — CBN, NDPR, SOC 2 all require it |
| Envelope Encryption | `models/encryption.go` | KEK + DEK separation, KMS-ready without code changes |
| DAG Scoping | `models/db.go` | `ScopedDB(orgID)` — every query scoped to tenant, cross-tenant leakage impossible |

---

<div align="center">

## Compliance Stack

</div>

<table>
<tr>
<td align="center" width="25%">

**CBN**

BVN verification at employee creation via Dojah API. KYC before any payroll data is stored.

</td>
<td align="center" width="25%">

**NDPR**

Append-only consent records per employee per type. Withdrawal creates a new row — nothing is deleted.

</td>
<td align="center" width="25%">

**SOC 2**

Daily evidence snapshots — audit events, migration version, payroll activity, security checks. One-click compliance report endpoint.

</td>
<td align="center" width="25%">

**Data Residency**

`data_region` on every org. Geo-fencing middleware rejects cross-region requests before any handler runs.

</td>
</tr>
</table>

---

<div align="center">

## Observability

</div>

```
Prometheus  ──→  /metrics endpoint (scraped every 15s)
    │
    ▼
Grafana Dashboard  (11 pre-built panels)
    ├── HTTP request rate + error %
    ├── API latency p95 by route
    ├── Monnify call latency p95 + success rate
    ├── Payroll processing duration p95
    ├── Auth failures by type (jwt_invalid / api_key_wrong)
    ├── Rate limit hits by key type
    ├── Webhook duplicate catch rate
    └── BVN verification success rate
```

Spin up the full stack in one command:

```bash
docker-compose up
# Grafana    → http://localhost:3000
# Prometheus → http://localhost:9090
# Metrics    → http://localhost:8080/metrics
```

---

<div align="center">

## Quick Start

</div>

```bash
# 1. Clone and configure
git clone https://github.com/obeej/go-payroll-engine.git
cd go-payroll-engine
cp .env.example .env

# 2. Start infrastructure
docker-compose up db redis -d

# 3. Seed the database
APP_MODE=seed go run cmd/api/main.go

# 4. Run API server
APP_MODE=api go run cmd/api/main.go

# 5. Run background worker (separate terminal)
APP_MODE=worker go run cmd/api/main.go
```

---

<div align="center">

## API Reference

</div>

| Method | Endpoint | Auth | Description |
|:---:|:---|:---:|:---|
| `POST` | `/api/v1/auth/login` | Public | Issue JWT |
| `POST` | `/api/v1/auth/refresh` | JWT | Refresh token |
| `POST` | `/api/v1/employees/` | JWT + Idempotency | Create employee (BVN verified + consent recorded) |
| `GET` | `/api/v1/employees/` | JWT | List employees (paginated) |
| `POST` | `/api/v1/payrolls/` | JWT + Idempotency | Initiate payroll batch — queued async |
| `GET` | `/api/v1/payrolls/:id` | JWT | Batch status + all items |
| `GET` | `/api/v1/analytics/predictive` | JWT | Cash flow forecast + risk level |
| `POST` | `/api/v1/consent/` | JWT | Record NDPR consent |
| `GET` | `/api/v1/consent/:employee_id` | JWT | Full consent history |
| `GET` | `/api/v1/compliance/report` | JWT `role=compliance` | 30-day SOC 2 / CBN evidence bundle |
| `POST` | `/api/v1/webhooks/monnify` | HMAC | Disbursement reconciliation |
| `GET` | `/healthz` | Public | Liveness probe |
| `GET` | `/readyz` | Public | Readiness probe (DB + Redis + encryption) |
| `GET` | `/metrics` | Public | Prometheus scrape |

---

<div align="center">

## Tech Stack

</div>

<div align="center">

![Go](https://img.shields.io/badge/Go-00ADD8?style=for-the-badge&logo=go&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-4169E1?style=for-the-badge&logo=postgresql&logoColor=white)
![Redis](https://img.shields.io/badge/Redis-DC382D?style=for-the-badge&logo=redis&logoColor=white)
![Docker](https://img.shields.io/badge/Docker-2496ED?style=for-the-badge&logo=docker&logoColor=white)
![Prometheus](https://img.shields.io/badge/Prometheus-E6522C?style=for-the-badge&logo=prometheus&logoColor=white)
![Grafana](https://img.shields.io/badge/Grafana-F46800?style=for-the-badge&logo=grafana&logoColor=white)
![GitHub Actions](https://img.shields.io/badge/GitHub_Actions-2088FF?style=for-the-badge&logo=github-actions&logoColor=white)

</div>

<br/>

| Layer | Technology |
|:---|:---|
| Language | Go 1.22+ |
| Web framework | Gin |
| ORM | GORM |
| Background jobs | Asynq (Redis-backed) |
| Migrations | golang-migrate (versioned SQL) |
| Auth | golang-jwt/jwt/v5 + bcrypt |
| Metrics | Prometheus + Grafana |
| Payment gateway | Monnify |
| KYC | Dojah (BVN verification) |

---

<div align="center">

## Testing

</div>

```bash
# Run all tests with race detector
go test ./... -race -count=1

# Run with coverage
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out
```

---

<div align="center">

## Project Structure

</div>

```
go-payroll-engine/
├── cmd/api/              # Entrypoint — api | worker | seed | collect-evidence
├── config/               # Prometheus scrape config + Grafana dashboard JSON
├── internal/
│   ├── api/
│   │   ├── handlers/     # HTTP handlers (auth, employees, payrolls, compliance...)
│   │   ├── middleware/   # JWT, rate limit, idempotency, bloom, residency, PII logger
│   │   └── routes.go     # Full middleware stack + route registration
│   ├── db/migrations/    # Versioned SQL migrations (000001 → 000003)
│   ├── integrations/
│   │   └── monnify/      # Bulk transfer + wallet balance client (mock-mode aware)
│   ├── models/           # GORM models + FSM + encryption + audit log
│   ├── observability/    # Prometheus metric definitions
│   ├── services/         # Payroll, analytics, BVN, SOC 2 evidence collector
│   └── workers/          # Asynq task handler + Redis + Asynq clients
└── pkg/money/            # Kobo type — integer arithmetic, banker's rounding
```

---

<div align="center">

## License

MIT — use it, fork it, learn from it.

<br/>

<img src="https://capsule-render.vercel.app/api?type=waving&color=0:24243e,50:302b63,100:0f0c29&height=120&section=footer" width="100%"/>

</div>
