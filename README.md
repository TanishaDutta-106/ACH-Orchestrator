# ACH Payment Retry Orchestrator

A production-quality ACH payment retry system built with Go, Temporal, and PostgreSQL.

## Phase Status

| Phase | Scope | Status |
|-------|-------|--------|
| 1 | Domain models, R-code routing, PostgreSQL schema, repository layer | ✅ Complete |
| 2 | Temporal workflows, Redis idempotency, HTTP API | ✅ Complete |
| 3 | NACHA file parsing and generation | 🔜 Planned |
| 4 | Observability, alerting, production hardening | 🔜 Planned |

## Phase 1 — Quick Start

### Prerequisites
- Go 1.22+
- Docker and Docker Compose

### Run PostgreSQL

```bash
docker compose up -d
```

This starts PostgreSQL and auto-runs the migration files in
`internal/db/migrations/` via the `docker-entrypoint-initdb.d` volume mount.

### Environment

```bash
cp .env.example .env
# Edit .env if your Postgres credentials differ from the defaults
```

### Run tests

Unit tests (no Docker required):
```bash
go test ./internal/domain/...
```

Integration tests (requires running Postgres):
```bash
DATABASE_URL="postgres://ach_user:ach_secret@localhost:5432/ach_orchestrator?sslmode=disable" \
  go test ./internal/db/... -v -tags integration
```

Or run everything:
```bash
go test ./...
```

## Architecture

```
internal/
├── domain/
│   ├── payment.go   Payment struct, PaymentState FSM
│   ├── rcode.go     NACHA R-code router (R01–R33)
│   └── rules.go     NACHA business rules, retry delays, state transition table
└── db/
    ├── repository.go  PostgreSQL operations (pgx/v5)
    └── migrations/    SQL schema files (applied in lexicographic order)
```

## NACHA R-Code Categories

| Category | R-Codes | Action |
|----------|---------|--------|
| Retryable | R01, R08, R09 | Re-present up to 2 times |
| NonRetryable | R02, R03, R04, R06, R07, R10, R11, R17–R24, R29–R33 | Stop; notify originator |
| ComplianceEscalation | R05, R14, R15, R16 | Human review required |
| Unknown | Any unrecognized code | Treated as NonRetryable (safe default) |

**R07 note:** Authorization Revoked by Customer is NonRetryable — not a
compliance escalation. Retrying an R07 is a NACHA rules violation.
