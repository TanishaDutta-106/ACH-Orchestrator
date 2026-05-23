# ACH Payment Retry Orchestrator

A production-quality ACH payment retry system built with Go, Temporal, and PostgreSQL.

## Phase Status

| Phase | Scope | Status |
|-------|-------|--------|
| 1 | Domain models, R-code routing, PostgreSQL schema, repository layer | ✅ Complete |
| 2 | Temporal workflows, Redis idempotency, HTTP API | 🔜 Planned |
| 3 | NACHA file parsing and generation | 🔜 Planned |
| 4 | Observability, alerting, production hardening | 🔜 Planned |

## Phase 1 — Quick Start

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
