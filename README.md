# ACH Payment Retry Orchestrator

A production-grade ACH payment processing engine that implements NACHA return-code routing, deterministic retry logic with representment limits, and durable workflow state management via Temporal. Built to handle the five canonical ACH failure scenarios — settle, non-retryable return, retryable exhaustion, compliance escalation, and idempotency rejection — with full audit trails.

**Stack:** Go 1.22 · Temporal · PostgreSQL · Redis · AWS ECS Fargate · Terraform · GitHub Actions

---

## Architecture Diagram

```
                         ┌─────────────────────────────────────────┐
                         │           AWS VPC (10.0.0.0/16)         │
                         │                                          │
   Internet              │  Public Subnets                          │
   ──────────►  ALB ─────┼──► ECS Fargate                          │
   (port 80)             │     ├── server  (chi REST API :8080)     │
                         │     └── worker  (Temporal worker)        │
                         │            │                             │
                         │  Private Subnets                         │
                         │     ├── RDS PostgreSQL  (db.t3.micro)    │
                         │     ├── ElastiCache Redis (cache.t3.micro)│
                         │     └── Temporal Cloud (free tier)       │
                         └─────────────────────────────────────────┘

Request flow:
  POST /payments      →  chi router  →  DB (create)   →  Temporal (start workflow)
  Temporal worker     →  Activities  →  DB (update)   →  Redis (idempotency check)
  NACHA return file   →  POST /payments/{id}/return   →  Temporal (signal workflow)
```

---

## State Machine

```
                    ┌─────────────┐
                    │  INITIATED  │  created via POST /payments
                    └──────┬──────┘
                           │ SubmitToACH activity
                           ▼
                    ┌─────────────┐
                    │   PENDING   │
                    └──────┬──────┘
                           │ ACH submission confirmed
                           ▼
                    ┌─────────────┐
                    │  SUBMITTED  │◄─────────────────────────────┐
                    └──────┬──────┘                              │
                           │                                     │
          ┌────────────────┼──────────────────┐                  │
          │                │                  │                  │
          ▼                ▼                  ▼                  │
     No return        ReturnSignal        ReturnSignal           │
     within 72h       (Retryable)         (NonRetryable /        │
          │                │               Compliance)           │
          ▼                ▼                  │                  │
    ┌──────────┐     ┌──────────┐             │                  │
    │ SETTLED  │     │ RETURNED │─────────────┘                  │
    └──────────┘     └──────┬───┘                                │
      (terminal)            │                                    │
                            │ count < MaxRepresentments          │
                            │ (R01, R08, R09)                    │
                            └────── 48h sleep ───────────────────┘
                                    new trace number
                                    idempotency check

                     count >= MaxRepresentments
                            │
              ┌─────────────┼──────────────────┐
              ▼             ▼                  ▼
 ┌──────────────────┐ ┌──────────────┐ ┌────────────────────────────┐
 │ FAILED_NON_      │ │ COMPLIANCE_  │ │ FAILED_RETRYABLE_EXHAUSTED │
 │ RETRYABLE        │ │ ESCALATION   │ │                            │
 └──────────────────┘ └──────────────┘ └────────────────────────────┘
       (terminal)          (terminal)              (terminal)
```

---

## R-Code Handling Table

Source of truth: `internal/domain/rcode.go`

| R-Code | Description | Category | Action |
|--------|-------------|----------|--------|
| R01 | Insufficient Funds | Retryable | Re-present up to MaxRepresentments (48h delay) |
| R02 | Account Closed | Non-Retryable | Fail immediately |
| R03 | No Account / Unable to Locate | Non-Retryable | Fail immediately |
| R04 | Invalid Account Number | Non-Retryable | Fail immediately |
| R05 | Unauthorized Debit (Corp SEC Code) | Compliance Escalation | Human review required |
| R06 | Returned per ODFI Request | Non-Retryable | Fail immediately |
| R07 | Authorization Revoked by Customer | Non-Retryable | Fail immediately — not compliance¹ |
| R08 | Payment Stopped | Retryable | Re-present up to MaxRepresentments |
| R09 | Uncollected Funds | Retryable | Re-present up to MaxRepresentments |
| R10 | Customer Advises Not Authorized | Non-Retryable | Fail immediately |
| R11 | Entry Not in Accordance with Authorization | Non-Retryable | Fail immediately |
| R12 | Branch Sold to Another DFI | Non-Retryable | Fail immediately |
| R13 | RDFI Not Qualified to Participate | Non-Retryable | Fail immediately |
| R14 | Representative Payee Deceased | Compliance Escalation | Human review required |
| R15 | Beneficiary / Account Holder Deceased | Compliance Escalation | Human review required |
| R16 | Account Frozen / OFAC Instruction | Compliance Escalation | Human review required — federal violation if retried |
| R17 | File Record Edit Criteria | Non-Retryable | Fix entry before resubmission |
| R18 | Improper Effective Entry Date | Non-Retryable | Fail immediately |
| R19 | Amount Field Error | Non-Retryable | Fail immediately |
| R20 | Non-Transaction Account | Non-Retryable | Fail immediately |
| R21 | Invalid Company Identification | Non-Retryable | Fail immediately |
| R22 | Invalid Individual ID Number | Non-Retryable | Fail immediately |
| R23 | Credit Entry Refused by Receiver | Non-Retryable | Fail immediately |
| R24 | Duplicate Entry | Non-Retryable | Fail immediately |
| R25 | Addenda Error | Non-Retryable | Fail immediately |
| R26 | Mandatory Field Error | Non-Retryable | Fail immediately |
| R27 | Trace Number Error | Non-Retryable | Fail immediately |
| R28 | Routing Number Check Digit Error | Non-Retryable | Fail immediately |
| R29 | Corporate Customer Advises Not Authorized | Non-Retryable | Fail immediately |
| R30 | RDFI Not Participant in Check Truncation | Non-Retryable | Fail immediately |
| R31 | Permissible Return Entry | Non-Retryable | Fail immediately |
| R32 | RDFI Non-Settlement | Non-Retryable | Fail immediately |
| R33 | Return of XCK Entry | Non-Retryable | Fail immediately |
| Unknown | Any unrecognized code | Non-Retryable | Safe default — alert on-call |

¹ R07 is NonRetryable, not ComplianceEscalation. Retrying an R07 constitutes an unauthorized debit — a NACHA rules violation. The correct action is to notify the customer and stop.

---

## NACHA Rules Implemented

- **Representment limit:** Maximum 2 re-presentations after the initial return (MaxRepresentments = 2). Hard-coded in `internal/domain/rules.go`. Attempting a third retry transitions directly to `FAILED_RETRYABLE_EXHAUSTED`.
- **Retry window:** 48-hour `workflow.Sleep` between representments for R01/R08/R09, matching NACHA's minimum waiting period guidance.
- **Idempotency:** Each submission and retry generates a unique trace number. Redis `SETNX` with 7-day TTL prevents duplicate debits if an activity fires more than once due to Temporal retry behavior.
- **Audit trail:** Every state transition is written to `audit_log` in the same PostgreSQL transaction as the state update. A state change without an audit record is impossible — `pgx.BeginTxFunc` rolls back both writes atomically on any failure.
- **Return code routing:** All R01–R33 codes map to exactly one category. Unknown codes default to NonRetryable — the safe choice is to halt and alert rather than retry blindly.
- **Terminal state enforcement:** `IsTransitionAllowed` in `rules.go` gates every state change before any database write. Illegal transitions return an error; the database is never touched.

---

## Tech Stack Justifications

**Why Go** — Go's goroutine model and compiled binary output make it well-suited for financial services backends where predictable GC pauses matter. The standard library's `net/http`, `encoding/json`, and `database/sql` provide everything needed without framework overhead. Static binaries simplify Docker images to under 50 MB with distroless base images, which matters for ECS Fargate cold start times.

**Why Temporal** — ACH payment processing is inherently a long-running, multi-step workflow: submission, waiting for return windows of 2–5 business days, then conditional retry or escalation. Temporal encodes this as durable code — if the worker crashes mid-workflow, execution resumes exactly where it left off on the next worker startup via event history replay. Temporal's signal mechanism maps directly to the NACHA return-file delivery model, and its built-in retry policies handle transient activity failures without custom backoff logic.

**Why pgx/v5** — pgx is the only Go PostgreSQL driver that supports the full PostgreSQL wire protocol natively, including `pgxpool` for connection pool management and type-safe scan targets. The standard `database/sql` interface obscures PostgreSQL-specific error codes needed to distinguish constraint violations from connectivity errors. pgx/v5 also has measurably lower allocation counts per query than `lib/pq`.

**Why chi** — chi is a lightweight HTTP router that uses only the standard library's `net/http` interfaces with no custom context types. Handlers are compatible with any middleware written for `net/http`. The pattern-matching syntax (`/payments/{id}`) compiles to a radix tree, not regex, making it faster for the handful of endpoints this service exposes.

**Why Redis for idempotency** — Trace numbers must be deduplicated across worker restarts, pod reschedules, and concurrent requests. Redis `SETNX` with a TTL provides atomic check-and-set in a single round trip with no distributed lock required. Storing trace numbers in PostgreSQL would work but adds a table scan or index lookup on the critical submission path; Redis keeps this under 1 ms even under load.

**Why shopspring/decimal** — Floating-point arithmetic is categorically wrong for monetary amounts. `float64` cannot represent `$0.10` exactly, and accumulated rounding errors in retry logic or representment calculations can produce penny discrepancies that fail NACHA amount reconciliation. `shopspring/decimal` uses arbitrary-precision base-10 arithmetic and its `String()` output is always correctly rounded, making serialization safe for NACHA amount field encoding.

---

## Local Setup

**Requires:** Go 1.22+, Docker, Docker Compose, `curl`, `jq`

```bash
# 1. Clone the repository
git clone https://github.com/TanishaDutta-106/ACH-Orchestrator && cd ACH-Orchestrator

# 2. Start all dependencies (PostgreSQL, Redis, Temporal)
docker compose up -d

# 3. Wait ~20 seconds for Temporal to initialize, then verify
open http://localhost:8088   # Temporal Web UI

# 4. Copy environment config
cp .env.example .env

# 5. Start the worker (terminal 1)
go run ./cmd/worker

# 6. Start the API server (terminal 2)
go run ./cmd/server

# 7. Verify health
curl http://localhost:8080/health
# {"status":"ok","postgres":"ok","redis":"ok","temporal":"ok"}
```

---

## AWS Deployment

> ⚠️ **Estimated cost: ~$85/month while running. Destroy immediately when done.**

### Prerequisites

- AWS CLI configured with sufficient IAM permissions
- Terraform >= 1.6
- Docker

### Deploy

```bash
# 1. Initialize Terraform
cd terraform && terraform init

# 2. Provision all infrastructure
terraform apply -auto-approve

# 3. Push Docker image to ECR
export ECR_URL=$(terraform output -raw ecr_repository_url)
aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin "$ECR_URL"
docker build --platform linux/amd64 -t "$ECR_URL:latest" .
docker push "$ECR_URL:latest"

# 4. Deploy to ECS
aws ecs update-service --cluster ach-orchestrator --service ach-orchestrator-server --force-new-deployment
aws ecs update-service --cluster ach-orchestrator --service ach-orchestrator-worker --force-new-deployment

# 5. Verify
curl http://$(terraform output -raw alb_dns_name)/health
```

### CI/CD (automated)

Push to `main` triggers the GitHub Actions pipeline:
1. **PRs:** `go test ./...` + `go vet` + `golint`
2. **Merge to main:** build → push ECR → force-deploy ECS

Required GitHub secret: `AWS_DEPLOY_ROLE_ARN`

---

## Teardown

```bash
cd terraform && terraform destroy -auto-approve
```

Removes all AWS resources: VPC, subnets, NAT gateway, ALB, ECS cluster, both ECS services, RDS instance, ElastiCache cluster, ECR repository, IAM roles, SSM parameters, and CloudWatch log groups. **There is no undo.**

---

## API Reference

### POST /payments — Submit payment and start workflow

```bash
curl -X POST http://localhost:8080/payments \
  -H "Content-Type: application/json" \
  -d '{
    "portfolio_id": "550e8400-e29b-41d4-a716-446655440000",
    "amount": "250.00",
    "account_number": "123456789",
    "routing_number": "021000021"
  }'
```

Response `201 Created`:
```json
{
  "id": "7f3b1c9d-...",
  "state": "INITIATED",
  "trace_number": "02100002100001",
  "created_at": "2024-01-15T10:30:00Z"
}
```

Validation rules: `amount` must be positive decimal · `account_number` must be 4–17 digits · `routing_number` must be exactly 9 digits · all fields required.

---

### GET /payments/{id} — Get current payment state

```bash
curl http://localhost:8080/payments/7f3b1c9d-...
```

Response `200 OK`:
```json
{
  "id": "7f3b1c9d-...",
  "portfolio_id": "550e8400-...",
  "amount": "250.00",
  "state": "SUBMITTED",
  "return_code": "",
  "representment_count": 0,
  "trace_number": "02100002100001",
  "created_at": "2024-01-15T10:30:00Z",
  "updated_at": "2024-01-15T10:30:01Z"
}
```

---

### POST /payments/{id}/return — Simulate NACHA return file

```bash
curl -X POST http://localhost:8080/payments/7f3b1c9d-.../return \
  -H "Content-Type: application/json" \
  -d '{"r_code": "R02", "trace_number": "02100002100001"}'
```

Response `200 OK`:
```json
{"status": "return signal sent"}
```

Signals the running Temporal workflow with the return code. The workflow transitions state based on the R-code category.

---

### GET /payments/{id}/audit — Full audit log

```bash
curl http://localhost:8080/payments/7f3b1c9d-.../audit
```

Response `200 OK`:
```json
[
  {
    "from_state": "INITIATED",
    "to_state": "PENDING",
    "reason": "workflow started",
    "created_at": "2024-01-15T10:30:00Z"
  },
  {
    "from_state": "PENDING",
    "to_state": "SUBMITTED",
    "reason": "ACH submission confirmed",
    "created_at": "2024-01-15T10:30:01Z"
  }
]
```

Every state transition is recorded in the same PostgreSQL transaction as the state update. Audit records cannot exist without a corresponding state change, and vice versa.

---

### GET /health — Dependency health check

```bash
curl http://localhost:8080/health
```

`200 OK` — all healthy:
```json
{"status":"ok","postgres":"ok","redis":"ok","temporal":"ok"}
```

`503 Service Unavailable` — partial failure:
```json
{"status":"degraded","postgres":"ok","redis":"error: dial tcp: connection refused","temporal":"ok"}
```

Each dependency is checked independently. A Redis outage does not mask a Temporal outage.

---

## Running Tests

### Unit tests (no dependencies required)

```bash
go test ./internal/domain/...
```

Covers: all R-code mappings, FSM transition validity, NACHA business rules.

### Integration tests (requires `docker compose up`)

```bash
export DATABASE_URL="postgres://ach_user:ach_secret@localhost:5432/ach_orchestrator?sslmode=disable"
export REDIS_ADDR="localhost:6379"
export TEMPORAL_ADDR="localhost:7233"

go test -v -timeout 120s -tags integration ./tests/...
```

### All tests

```bash
go test -v -timeout 120s ./...
```

### Linting

```bash
go vet ./...
golint -set_exit_status ./...
```

### R01 exhaustion test (requires Temporal time-skipping)

The R01 retry exhaustion scenario involves two 48-hour `workflow.Sleep` calls. Run locally with Temporal's dev server time-skipping to avoid a 4-day wait:

```bash
temporal server start-dev --headless
go test -v -timeout 600s -run "TestIntegration_R01Exhaustion" ./tests/integration/...
```

---

## Demo Scenarios

### Scenario 1 — Happy path (settles after 72h timeout)

```bash
ID=$(curl -s -X POST http://localhost:8080/payments \
  -H "Content-Type: application/json" \
  -d '{"portfolio_id":"550e8400-e29b-41d4-a716-446655440000","amount":"100.00","account_number":"123456789","routing_number":"021000021"}' \
  | jq -r .id)

sleep 2 && curl -s http://localhost:8080/payments/$ID | jq .state
# "SUBMITTED"

# No return signal → workflow settles after 72h Temporal timer
# Use Temporal dev server with time-skipping to test locally without waiting
```

### Scenario 2 — Non-retryable return (R02)

```bash
ID=$(curl -s -X POST http://localhost:8080/payments \
  -H "Content-Type: application/json" \
  -d '{"portfolio_id":"550e8400-e29b-41d4-a716-446655440000","amount":"100.00","account_number":"123456789","routing_number":"021000021"}' \
  | jq -r .id)

TRACE=$(curl -s http://localhost:8080/payments/$ID | jq -r .trace_number)
sleep 2

curl -X POST http://localhost:8080/payments/$ID/return \
  -H "Content-Type: application/json" \
  -d "{\"r_code\":\"R02\",\"trace_number\":\"$TRACE\"}"

sleep 1 && curl -s http://localhost:8080/payments/$ID | jq '{state,return_code}'
# {"state":"FAILED_NON_RETRYABLE","return_code":"R02"}
```

### Scenario 3 — Compliance escalation (R05)

```bash
# Same as Scenario 2 with r_code: "R05"
# Expected: {"state":"COMPLIANCE_ESCALATION","return_code":"R05"}
```

### Scenario 4 — Compliance escalation (R16 — OFAC)

```bash
# Same as Scenario 2 with r_code: "R16"
# Expected: {"state":"COMPLIANCE_ESCALATION","return_code":"R16"}
# Note: R16 indicates an OFAC sanctions freeze — retrying is a federal violation
```

### Scenario 5 — Idempotency rejection

```bash
# Submit payment — note the trace number from response
curl -s -X POST http://localhost:8080/payments \
  -H "Content-Type: application/json" \
  -d '{"portfolio_id":"550e8400-e29b-41d4-a716-446655440000","amount":"100.00","account_number":"123456789","routing_number":"021000021"}'

# Attempt to resubmit with same trace number
# Expected: 409 {"error":"trace number already exists"}
```

### Scenario 6 — Health check with dependency down

```bash
docker compose stop redis
curl http://localhost:8080/health
# 503 {"status":"degraded","postgres":"ok","redis":"error: ...","temporal":"ok"}

docker compose start redis
```

---

## Repository Topics

`go` `temporal` `ach` `nacha` `payments` `fintech` `distributed-systems` `postgresql` `redis`