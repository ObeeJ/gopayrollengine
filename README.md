<div align="center">
  <img src="https://capsule-render.vercel.app/api?type=waving&color=0:000000,100:1a1a2e&height=160&section=header&text=go-payroll-engine&fontSize=42&fontColor=ffffff&fontAlignY=45&desc=Payroll%20Disbursement%20Infrastructure%20for%20FinTech&descSize=16&descColor=888888&descAlignY=65" width="100%" />
</div>

<br />

<div align="center">

[![CI](https://github.com/ObeeJ/gopayrollengine/actions/workflows/ci.yml/badge.svg)](https://github.com/ObeeJ/gopayrollengine/actions)&nbsp;
![Go](https://img.shields.io/badge/Go_1.22+-00ADD8?style=flat-square&logo=go&logoColor=white)&nbsp;
![Postgres](https://img.shields.io/badge/PostgreSQL_15-4169E1?style=flat-square&logo=postgresql&logoColor=white)&nbsp;
![Redis](https://img.shields.io/badge/Redis_7-DC382D?style=flat-square&logo=redis&logoColor=white)&nbsp;
![License](https://img.shields.io/badge/MIT-white?style=flat-square)

</div>

<br />

<div align="center">
<p><strong>Async bulk salary disbursement engine built to global FinTech production standards.</strong><br/>
PCI DSS · NDPR · SOC 2 · CBN — compliance by design, not by retrofit.</p>
</div>

<br />

---

<br />

## Overview

go-payroll-engine is a backend system for processing bulk payroll disbursements through Monnify. It handles the full lifecycle — from payroll batch creation through background processing, real-time webhook reconciliation, and compliance evidence generation.

The codebase is designed as a reference implementation of what production FinTech infrastructure looks like: every security control, compliance requirement, and observability concern addressed at the architecture level rather than patched in later.

<br />

---

<br />

## Architecture

<br />

```
  Client Request
       │
       ▼
  ┌─────────────────────────────────────────────────────┐
  │  API Layer                                          │
  │                                                     │
  │  SecurityHeaders → BodyLimit → Logger → Metrics     │
  │  RateLimit → JWTAuth → TenantScope → DataResidency  │
  └────────────────────────┬────────────────────────────┘
                           │
                           ▼
  ┌─────────────────────────────────────────────────────┐
  │  Service Layer                                      │
  │                                                     │
  │  CreatePayroll → DB Transaction → Redis Enqueue     │
  └────────────────────────┬────────────────────────────┘
                           │
                           ▼
  ┌─────────────────────────────────────────────────────┐
  │  Worker Layer                          (Asynq)      │
  │                                                     │
  │  FSM Transition → Employee Hash Map → Monnify API   │
  └────────────────────────┬────────────────────────────┘
                           │
                           ▼
  ┌─────────────────────────────────────────────────────┐
  │  Webhook Handler                                    │
  │                                                     │
  │  HMAC Verify → Bloom Filter → FSM → Atomic Counter  │
  │  → Audit Log → Reconcile Parent Payroll             │
  └─────────────────────────────────────────────────────┘
```

<br />

---

<br />

## Security

<br />

<table>
<thead>
<tr>
<th width="25%">Authentication</th>
<th width="25%">Encryption</th>
<th width="25%">Transport</th>
<th width="25%">Abuse Prevention</th>
</tr>
</thead>
<tbody>
<tr>
<td valign="top">

JWT with org-scoped claims

RBAC role gates

bcrypt cost-12

Timing-safe key comparison

</td>
<td valign="top">

AES-256-GCM on all PII fields

Transparent GORM serializer

Envelope encryption (KEK/DEK)

KMS-ready interface

</td>
<td valign="top">

OWASP security headers

1 MB body size limit

Non-root Docker image

Production startup guards

</td>
<td valign="top">

Token bucket rate limiting

Idempotency keys (Redis, 24h)

Bloom filter deduplication

HMAC-SHA512 webhook verification

</td>
</tr>
</tbody>
</table>

<br />

---

<br />

## Data Integrity

<br />

| Pattern | File | Guarantee |
|:---|:---|:---|
| Finite State Machine | `models/models.go` | Illegal status transitions rejected before any DB write |
| Atomic Counter | `models/models.go` | Webhook reconciliation fires exactly once per batch |
| Hash Map | `workers/payroll_worker.go` | O(1) employee lookup — eliminates N+1 query pattern |
| Bloom Filter | `middleware/bloom.go` | ~99% of duplicate webhooks caught before DB read |
| Append-Only Log | `audit_events` table | Immutable record of every state change — no UPDATE, no DELETE |
| Idempotency Map | `middleware/idempotency.go` | Network retry cannot create duplicate payroll batch |
| Weighted Sliding Window | `services/analytics_service.go` | Cash flow forecast biased toward recent data |
| Envelope Encryption | `models/encryption.go` | PII encrypted at rest, decrypted transparently on read |
| DAG Tenant Scoping | `models/db.go` | Every query scoped to org — cross-tenant leakage structurally impossible |

<br />

---

<br />

## Compliance

<br />

<table>
<thead>
<tr>
<th width="25%">CBN</th>
<th width="25%">NDPR</th>
<th width="25%">SOC 2</th>
<th width="25%">Data Residency</th>
</tr>
</thead>
<tbody>
<tr>
<td valign="top">

BVN verification via Dojah at employee creation.

KYC recorded before any payroll data is stored.

Response hash stored — not the BVN itself.

</td>
<td valign="top">

Append-only consent records per employee per type.

Withdrawal creates a new row — history is never mutated.

Consent expiry enforced at query time.

</td>
<td valign="top">

Daily evidence snapshots to JSON.

One-click compliance report endpoint.

Migration version, audit counts, security checks all captured.

</td>
<td valign="top">

`data_region` field on every organization.

Geo-fencing middleware rejects cross-region requests.

Default region `ng` — zero breaking changes for existing orgs.

</td>
</tr>
</tbody>
</table>

<br />

---

<br />

## Observability

<br />

```
  /metrics  ──→  Prometheus (scrape interval: 15s)
                      │
                      ▼
               Grafana Dashboard
                      │
       ┌──────────────┼──────────────┐
       │              │              │
  HTTP Layer     Payment Layer   Security Layer
       │              │              │
  request rate    Monnify p95    auth failures
  error rate %    success rate   rate limit hits
  latency p95     batch duration  webhook dupes
```

<br />

Start the full observability stack:

```bash
docker-compose up
```

| Service | URL |
|:---|:---|
| API | http://localhost:8080 |
| Grafana | http://localhost:3000 |
| Prometheus | http://localhost:9090 |
| Metrics | http://localhost:8080/metrics |

<br />

---

<br />

## Getting Started

<br />

**Prerequisites:** Go 1.22+, Docker, PostgreSQL 15, Redis 7

```bash
# Clone
git clone https://github.com/ObeeJ/gopayrollengine.git
cd go-payroll-engine

# Configure
cp .env.example .env
# Edit .env — set JWT_SECRET, ENCRYPTION_KEK, MONNIFY credentials

# Start infrastructure
docker-compose up db redis -d

# Seed database
APP_MODE=seed go run cmd/api/main.go

# Start API server
APP_MODE=api go run cmd/api/main.go

# Start background worker (separate terminal)
APP_MODE=worker go run cmd/api/main.go
```

<br />

---

<br />

## API

<br />

**Authentication**

| Method | Path | Description |
|:---|:---|:---|
| `POST` | `/api/v1/auth/login` | Issue JWT |
| `POST` | `/api/v1/auth/refresh` | Refresh token |

**Employees**

| Method | Path | Description |
|:---|:---|:---|
| `POST` | `/api/v1/employees/` | Create employee — BVN verified, consent recorded, PII encrypted |
| `GET` | `/api/v1/employees/` | List employees (paginated) |

**Payroll**

| Method | Path | Description |
|:---|:---|:---|
| `POST` | `/api/v1/payrolls/` | Initiate batch — idempotent, queued async |
| `GET` | `/api/v1/payrolls/:id` | Batch status and all disbursement items |

**Analytics & Compliance**

| Method | Path | Description |
|:---|:---|:---|
| `GET` | `/api/v1/analytics/predictive` | Cash flow forecast with risk level |
| `POST` | `/api/v1/consent/` | Record NDPR consent |
| `GET` | `/api/v1/consent/:employee_id` | Full consent history |
| `GET` | `/api/v1/compliance/report` | 30-day SOC 2 / CBN evidence bundle — requires `role=compliance` |

**Infrastructure**

| Method | Path | Description |
|:---|:---|:---|
| `POST` | `/api/v1/webhooks/monnify` | Disbursement reconciliation — HMAC verified |
| `GET` | `/healthz` | Liveness probe |
| `GET` | `/readyz` | Readiness — checks DB, Redis, encryption key |
| `GET` | `/metrics` | Prometheus scrape endpoint |

<br />

---

<br />

## Stack

<br />

<div align="center">

![Go](https://img.shields.io/badge/Go-00ADD8?style=for-the-badge&logo=go&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-4169E1?style=for-the-badge&logo=postgresql&logoColor=white)
![Redis](https://img.shields.io/badge/Redis-DC382D?style=for-the-badge&logo=redis&logoColor=white)
![Docker](https://img.shields.io/badge/Docker-2496ED?style=for-the-badge&logo=docker&logoColor=white)
![Prometheus](https://img.shields.io/badge/Prometheus-E6522C?style=for-the-badge&logo=prometheus&logoColor=white)
![Grafana](https://img.shields.io/badge/Grafana-F46800?style=for-the-badge&logo=grafana&logoColor=white)
![GitHub Actions](https://img.shields.io/badge/GitHub_Actions-2088FF?style=for-the-badge&logo=github-actions&logoColor=white)

</div>

<br />

| Layer | Technology |
|:---|:---|
| Language | Go 1.22+ |
| Web framework | Gin |
| ORM | GORM |
| Background jobs | Asynq |
| Migrations | golang-migrate |
| Auth | golang-jwt/jwt/v5 |
| Metrics | Prometheus + Grafana |
| Payment gateway | Monnify |
| KYC | Dojah |

<br />

---

<br />

## Project Structure

<br />

```
go-payroll-engine/
├── cmd/api/                    # Entrypoint — api | worker | seed | collect-evidence
├── config/                     # Prometheus config, Grafana dashboard
├── internal/
│   ├── api/
│   │   ├── handlers/           # auth, employees, payrolls, webhooks, compliance
│   │   ├── middleware/         # jwt, ratelimit, idempotency, bloom, residency, logger
│   │   └── routes.go
│   ├── db/
│   │   └── migrations/         # 000001 → 000003 versioned SQL
│   ├── integrations/
│   │   └── monnify/            # bulk transfer + wallet balance (mock-aware)
│   ├── models/                 # GORM models, FSM, encryption, audit log
│   ├── observability/          # Prometheus metric definitions
│   ├── services/               # payroll, analytics, BVN, SOC 2 collector
│   └── workers/                # Asynq handlers, Redis client, Asynq client
└── pkg/
    └── money/                  # Kobo type — integer arithmetic, banker's rounding
```

<br />

---

<br />

## Testing

<br />

```bash
# All tests with race detector
go test ./... -race -count=1

# With coverage report
go test ./... -coverprofile=coverage.out && go tool cover -html=coverage.out
```

<br />

---

<br />

<div align="center">

MIT License

<br />

<img src="https://capsule-render.vercel.app/api?type=waving&color=0:1a1a2e,100:000000&height=100&section=footer" width="100%" />

</div>
