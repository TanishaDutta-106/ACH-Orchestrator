# ACH Payment Retry Orchestrator

A production-grade ACH payment processing engine that implements NACHA return-code routing, deterministic retry logic with representment limits, and durable workflow state management. Built to handle the five canonical ACH failure scenarios (settle, non-retryable return, retryable exhaustion, compliance escalation, idempotency rejection) with full audit trails.

GitHub topics: `go` `temporal` `ach` `nacha` `payments` `fintech` `distributed-systems` `postgresql` `redis`

---

## Architecture Diagram

```
                         ┌─────────────────────────────────────────┐
                         │              AWS VPC (10.0.0.0/16)      │
                         │                                          │
   Internet              │  Public Subnets                          │
   ──────────►  ALB ─────┼──► ECS Fargate                          │
   (port 80)             │     ├── server (chi REST API :8080)      │
                         │     └── worker (Temporal worker)         │
                         │            │                             │
                         │  Private Subnets                         │
                         │     ├── RDS PostgreSQL (db.t3.micro)     │
                         │     ├── ElastiCache Redis (cache.t3.micro)│
                         │     └── Temporal (Cloud or EC2)          │
                         └─────────────────────────────────────────┘

Request flow:
  POST /payments  →  chi router  →  DB (create)  →  Temporal (start workflow)
  Temporal worker →  Activities  →  DB (update)  →  Redis (idempotency check)
  NACHA return    →  POST /payments/{id}/return   →  Temporal (signal workflow)
```

---

## State Machine Diagram

```
                    ┌─────────────┐
                    │  INITIATED  │  (created via POST /payments)
                    └──────┬──────┘
                           │ activity: submit to ACH network
                           ▼
                    ┌─────────────┐
                    │   PENDING   │◄──────────────────────────┐
                    └──────┬──────┘                           │
                           │ ACH submission confirmed          │ retry (R01/R09)
                           ▼                                  │ up to MaxRepresentments
                    ┌─────────────┐                           │
                    │  SUBMITTED  │                           │
                    └──────┬──────┘                           │
                           │                                  │
              ┌────────────┼────────────────┐                 │
              │            │                │                 │
              ▼            ▼                ▼                 │
         No return    Return signal    Return signal          │
              │        (retryable)    (non-retryable          │
              │            │          / compliance)           │
              ▼            ▼                │                 │
        ┌──────────┐ ┌──────────┐          │                 │
        │ SETTLED  │ │ RETURNED │──────────┘                 │
        └──────────┘ └──────┬───┘          │                 │
          (terminal)        │              │                 │
                            │ R01/R09      │ R02-R04,R06-    │
                            │ count < max  │ R08,R10-R16,    │
                            └─────────────┘ R20,R23,R29     │
                                           │                 │
                            ┌──────────────┘                 │
                            │                                │
              ┌─────────────┼──────────────┐                 │
              ▼             ▼              ▼                  │
 ┌──────────────────┐ ┌──────────────┐ ┌────────────────────┴──────┐
 │ FAILED_NON_      │ │ COMPLIANCE_  │ │ FAILED_RETRYABLE_EXHAUSTED │
 │ RETRYABLE        │ │ ESCALATION   │ │ (representments exhausted) │
 └──────────────────┘ └──────────────┘ └────────────────────────────┘
       (terminal)          (terminal)              (terminal)
```

---

## R-Code Handling Table

| R-Code | Description                        | Category        | Action                          |
|--------|------------------------------------|-----------------|---------------------------------|
| R01    | Insufficient Funds                 | Retryable       | Re-present up to MaxRepresentments (24h delay) |
| R02    | Account Closed                     | Non-Retryable   | Fail immediately                |
| R03    | No Account / Unable to Locate      | Non-Retryable   | Fail immediately                |
| R04    | Invalid Account Number             | Non-Retryable   | Fail immediately                |
| R05    | Unauthorized Debit (Corp)          | Compliance      | Escalate to compliance team     |
| R06    | Returned per ODFI Request          | Non-Retryable   | Fail immediately                |
| R07    | Authorization Revoked              | Non-Retryable   | Fail immediately                |
| R08    | Payment Stopped                    | Non-Retryable   | Fail immediately                |
| R09    | Uncollected Funds                  | Retryable       | Re-present up to MaxRepresentments |
| R10    | Customer Advises Not Authorized    | Non-Retryable   | Fail immediately                |
| R11    | Check Truncation Entry Return      | Non-Retryable   | Fail immediately                |
| R12    | Branch Sold to Another DFI         | Non-Retryable   | Fail immediately                |
| R13    | Invalid ACH Routing Number         | Non-Retryable   | Fail immediately                |
| R14    | Representative Payee Deceased      | Non-Retryable   | Fail immediately                |
| R15    | Beneficiary / Account Deceased     | Non-Retryable   | Fail immediately                |
| R16    | Account Frozen                     | Non-Retryable   | Fail immediately                |
| R17    | File Record Edit Criteria          | Compliance      | Escalate to compliance team     |
| R20    | Non-Transaction Account            | Non-Retryable   | Fail immediately                |
| R23    | Credit Entry Refused by Receiver   | Non-Retryable   | Fail immediately                |
| R29    | Corporate Customer Advises Not Auth| Non-Retryable   | Fail immediately                |

---

## NACHA Rules Implemented

- **94-character fixed-width record format** — each line is exactly 94 characters
- **Record type identification** — `1` (File Header), `5` (Batch Header), `6` (Entry Detail), `8` (Batch Control), `9` (File Control)
- **Amount field decoding** — 10-digit zero-padded integer with implied 2 decimal places (`0000010000` = `$100.00`)
- **Trace number extraction** — columns 79–94 of the Entry Detail record
- **Line ending normalization** — `strings.TrimRight(raw, "\r")` applied before length check; space-trimming is intentionally omitted to prevent short-record false negatives
- **Return entry detection** — transaction code `21` (checking debit return) and `26` (savings debit return)
- **Batch hash validation** — routing number sum verification on Batch Control record

---

## Tech Stack Justifications

**Why Go** — Go's goroutine model and compiled binary output make it ideal for financial services backends where sub-millisecond latency and predictable GC pauses matter. The standard library's `net/http`, `encoding/json`, and `database/sql` provide everything needed without framework overhead. Static binaries simplify Docker images to under 50 MB with distroless base images, which matters for container startup time in ECS Fargate cold starts.

**Why Temporal** — ACH payment processing is inherently a long-running, multi-step workflow: submission, waiting for return windows (2–5 business days), conditional retry or escalation. Temporal encodes this as durable code — if the worker crashes mid-workflow, execution resumes exactly where it left off on the next worker. Temporal's signal mechanism maps directly to the NACHA return-code delivery model, and its built-in retry policies handle transient activity failures without custom backoff logic.

**Why pgx/v5** — pgx is the only Go PostgreSQL driver that supports the full PostgreSQL wire protocol natively, including `pgx.CopyFrom` for bulk inserts, `pgxpool` for connection pool management, and type-safe scan targets. The standard `database/sql` interface adds an abstraction layer that obscures PostgreSQL-specific error codes (needed for distinguishing constraint violations from connectivity errors). pgx/v5 also has measurably lower allocation counts per query than `lib/pq`.

**Why chi** — chi is a lightweight HTTP router that uses only the standard library's `net/http` interfaces, with no custom context types. This means handlers are compatible with any middleware written for `net/http` and the router adds approximately 1 µs per request. The pattern-matching syntax (`/payments/{id}`) is familiar from other routers but compiles to a radix tree, not regex, making it faster for the handful of endpoints this service exposes.

**Why Redis for idempotency** — Trace numbers must be deduplicated across worker restarts, pod reschedules, and concurrent requests. Redis `SETNX` with a TTL provides atomic check-and-set in a single round trip with no distributed lock required. Storing trace numbers in PostgreSQL would work but adds a table scan or index lookup on the critical submission path; Redis keeps this under 1 ms even under load.

**Why shopspring/decimal** — Floating-point arithmetic is categorically wrong for monetary amounts. `float64` cannot represent `$0.10` exactly, and accumulated rounding errors in retry logic or representment calculations can produce penny discrepancies that fail NACHA amount reconciliation. `shopspring/decimal` uses arbitrary-precision base-10 arithmetic and its `String()` output is always correctly rounded, making serialization safe for NACHA amount field encoding.

---

## Local Setup

Requires: Docker, Docker Compose, Go 1.22, `curl`

```bash
# 1. Clone and start all dependencies
git clone https://github.com/yourorg/ach-orchestrator && cd ach-orchestrator
docker compose up -d

# 2. Wait for Temporal UI to be available (~20 seconds)
open http://localhost:8088   # Temporal Web UI

# 3. Start the worker (separate terminal)
go run ./cmd/worker

# 4. Start the API server (separate terminal)
go run ./cmd/server

# 5. Verify health
curl http://localhost:8080/health
# {"status":"ok","postgres":"ok","redis":"ok","temporal":"ok"}
```

---

## AWS Deployment Instructions

### Prerequisites

- AWS CLI configured with an account that can create IAM, ECS, RDS, and ElastiCache resources
- Terraform >= 1.6 installed
- Docker installed for image builds
- An ECR repository (created by Terraform in step 2)

### Steps

```bash
# 1. Initialize Terraform
cd terraform
terraform init

# 2. Apply infrastructure (creates ECR, VPC, RDS, Redis, ECS, ALB)
terraform apply -auto-approve

# 3. Note the ECR URL from output
export ECR_URL=$(terraform output -raw ecr_repository_url)

# 4. Build and push the image
aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin "$ECR_URL"
docker build --platform linux/amd64 -t "$ECR_URL:latest" .
docker push "$ECR_URL:latest"

# 5. Force ECS to pull the new image
aws ecs update-service --cluster ach-orchestrator --service ach-orchestrator-server --force-new-deployment
aws ecs update-service --cluster ach-orchestrator --service ach-orchestrator-worker --force-new-deployment

# 6. Verify deployment
ALB=$(terraform output -raw alb_dns_name)
curl http://$ALB/health
```

### GitHub Actions (automated path)

Push to `main` triggers the CI/CD pipeline automatically:

1. PRs: `go test ./...` + `go vet` + `golint`
2. Merge to main: build → push ECR → force-deploy ECS

Add these secrets to your GitHub repository:
- `AWS_DEPLOY_ROLE_ARN` — IAM role with ECS deploy and ECR push permissions

---

## Teardown Instructions

> ⚠️ **This project costs ~$85/month while running. Run `destroy` immediately when done.**

```bash
cd terraform
terraform destroy -auto-approve
```

This single command removes **all** AWS resources: VPC, subnets, NAT gateway, ALB, ECS cluster, both ECS services, RDS instance, ElastiCache cluster, ECR repository, IAM roles, SSM parameters, and CloudWatch log groups.

**There is no undo.** After destroy, all data in RDS and all Docker images in ECR are permanently deleted.

---

## API Reference

### POST /payments — Create payment and start workflow

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

---

### GET /payments/{id} — Get payment state

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

### POST /payments/{id}/return — Simulate NACHA return

```bash
curl -X POST http://localhost:8080/payments/7f3b1c9d-.../return \
  -H "Content-Type: application/json" \
  -d '{"r_code": "R02", "trace_number": "02100002100001"}'
```

Response `200 OK`:
```json
{"status": "return signal sent"}
```

---

### GET /health — Health check

```bash
curl http://localhost:8080/health
```

Response `200 OK` (all healthy):
```json
{"status":"ok","postgres":"ok","redis":"ok","temporal":"ok"}
```

Response `503 Service Unavailable` (partial failure):
```json
{"status":"degraded","postgres":"ok","redis":"error: dial tcp: connection refused","temporal":"ok"}
```

---

## Running Tests

### Unit tests only (no dependencies required)

```bash
go test -v -run "^TestUnit" ./test/...
```

### Integration tests (requires docker compose up)

```bash
export TEST_DB_URL="postgres://achuser:password@localhost:5432/ach_db?sslmode=disable"
export TEST_REDIS_ADDR="localhost:6379"
export TEST_TEMPORAL_ADDR="localhost:7233"

# All integration tests except R01 exhaustion (24h delay)
go test -v -timeout 120s -run "^TestIntegration" -skip "R01Exhaustion" ./test/integration/...
```

### All tests (unit + integration)

```bash
go test -v -timeout 120s ./...
```

### Linting

```bash
go vet ./...
golint -set_exit_status ./...
```

### R01 exhaustion test (local only, requires Temporal time-skipping)

```bash
# Start Temporal dev server with time-skipping support
temporal server start-dev --headless

go test -v -timeout 600s -run "TestIntegration_R01Exhaustion" ./test/integration/...
```

---

## Demo Scenarios

### Scenario 1 — Happy path (settles)

```bash
# Create payment
ID=$(curl -s -X POST http://localhost:8080/payments \
  -H "Content-Type: application/json" \
  -d '{"portfolio_id":"550e8400-e29b-41d4-a716-446655440000","amount":"100.00","account_number":"123456789","routing_number":"021000021"}' \
  | jq -r .id)

# Poll state — expect SUBMITTED within 2s
sleep 2 && curl http://localhost:8080/payments/$ID | jq .state
# "SUBMITTED"

# No return signal sent → workflow settles automatically
sleep 3 && curl http://localhost:8080/payments/$ID | jq .state
# "SETTLED"
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

sleep 1 && curl http://localhost:8080/payments/$ID | jq '{state,return_code}'
# {"state":"FAILED_NON_RETRYABLE","return_code":"R02"}
```

### Scenario 3 — Compliance escalation (R05)

```bash
# Same as Scenario 2 but with r_code: "R05"
# Expected final state: COMPLIANCE_ESCALATION
```

### Scenario 4 — Idempotency rejection

```bash
# Submit the same payment twice — second returns 409 Conflict
curl -X POST http://localhost:8080/payments \
  -d '{"portfolio_id":"550e8400-...","amount":"100.00","account_number":"123456789","routing_number":"021000021"}'

# Resubmit same trace number (returned from first call)
# → 409 {"error":"trace number already exists"}
```

### Scenario 5 — Health check with Redis down

```bash
docker compose stop redis
curl http://localhost:8080/health
# 503 {"status":"degraded","postgres":"ok","redis":"error: ...","temporal":"ok"}

docker compose start redis
```