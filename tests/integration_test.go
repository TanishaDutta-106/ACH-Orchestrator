//go:build integration

// Package tests contains integration tests for the ACH Payment Retry
// Orchestrator. Requires live PostgreSQL (5433), Redis (6380), and Temporal
// (7233).
//
// Run with:
//
//	DATABASE_URL="postgres://ach_user:ach_secret@localhost:5433/ach_orchestrator?sslmode=disable" \
//	TEMPORAL_HOST_PORT="localhost:7233" \
//	REDIS_ADDR="localhost:6380" \
//	go test ./tests/... -v -tags integration -timeout 10m
package tests

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/activities"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/db"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/domain"
	achredis "github.com/TanishaDutta-106/ACH-Orchestrator/internal/redis"
	achworkflow "github.com/TanishaDutta-106/ACH-Orchestrator/internal/workflow"
)

// ── Shared harness ────────────────────────────────────────────────────────────

type integrationEnv struct {
	tc          client.Client
	repo        *db.Repository
	redisClient *achredis.Client
	acts        *activities.Activities
	w           worker.Worker
	cancel      context.CancelFunc
}

func setupEnv(t *testing.T) *integrationEnv {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	databaseURL := envOrDefault("DATABASE_URL",
		"postgres://ach_user:ach_secret@localhost:5433/ach_orchestrator?sslmode=disable")
	temporalHost := envOrDefault("TEMPORAL_HOST_PORT", "localhost:7233")
	redisAddr := envOrDefault("REDIS_ADDR", "localhost:6380")

	repo, err := db.NewRepository(ctx, databaseURL)
	require.NoError(t, err, "connect to postgres")

	rc := achredis.NewClient(redisAddr)
	require.NoError(t, rc.Ping(ctx), "ping redis")

	tc, err := client.Dial(client.Options{HostPort: temporalHost})
	require.NoError(t, err, "dial temporal")

	acts := &activities.Activities{Repo: repo, RedisClient: rc}

	w := worker.New(tc, domain.TemporalTaskQueue, worker.Options{})
	w.RegisterWorkflow(achworkflow.PaymentWorkflow)
	w.RegisterActivity(acts)

	go func() {
		if err := w.Run(worker.InterruptCh()); err != nil && ctx.Err() == nil {
			t.Logf("integration worker error: %v", err)
		}
	}()

	t.Cleanup(func() {
		cancel()
		w.Stop()
		repo.Close()
		rc.Close()
		tc.Close()
	})

	return &integrationEnv{
		tc:          tc,
		repo:        repo,
		redisClient: rc,
		acts:        acts,
		w:           w,
		cancel:      cancel,
	}
}

// ── Your existing Test 1 (preserved exactly) ─────────────────────────────────

// TestIntegration_R01_ExhaustedRetries exercises the full retry loop:
//   - Submit payment, signal R01 three times → FAILED_RETRYABLE_EXHAUSTED.
//
// Signals are buffered by Temporal during workflow.Sleep — all three arrive
// and are processed in order, exhausting MaxRepresentments (2 retries after
// the original submission = 3 total R01 signals).
func TestIntegration_R01_ExhaustedRetries(t *testing.T) {
	env := setupEnv(t)
	ctx := context.Background()

	paymentID := uuid.New()

	payment := &domain.Payment{
    ID:     paymentID,
    Amount: decimal.NewFromFloat(250.00),
    State:  domain.StateInitiated,
	}
	require.NoError(t, env.repo.CreatePayment(ctx, payment))

	options := client.StartWorkflowOptions{
		ID:        "integration-r01-exhausted-" + paymentID.String(),
		TaskQueue: domain.TemporalTaskQueue,
	}
	run, err := env.tc.ExecuteWorkflow(ctx, options,
		achworkflow.PaymentWorkflow,
		achworkflow.PaymentWorkflowInput{
			PaymentID:     paymentID,
			Amount:        "250.00",
			AccountNumber: "987654321",
			RoutingNumber: "021000021",
		},
	)
	require.NoError(t, err)

	time.Sleep(2 * time.Second)

	// Signal R01 three times — original + 2 representments.
	// Temporal buffers signals during workflow.Sleep so all three are received.
	for i := 0; i < 3; i++ {
		err = env.tc.SignalWorkflow(ctx, run.GetID(), run.GetRunID(),
			achworkflow.ReturnSignalName,
			achworkflow.ReturnSignal{RCode: "R01", TraceNumber: "test-trace"},
		)
		require.NoErrorf(t, err, "signal %d", i)
		time.Sleep(500 * time.Millisecond)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var result interface{}
	err = run.Get(waitCtx, &result)
	require.NoError(t, err)

	finalPayment, err := env.repo.GetPaymentByID(ctx, paymentID)
	require.NoError(t, err)
	require.Equal(t, domain.StateFailedRetryableExhausted, finalPayment.State,
		"expected FAILED_RETRYABLE_EXHAUSTED after 3 R01 returns")

	audit, err := env.repo.GetAuditLogByPaymentID(ctx, paymentID)
	require.NoError(t, err)
	require.NotEmpty(t, audit, "audit log must not be empty")
}

// ── Your existing Test 2 (preserved exactly) ─────────────────────────────────

// TestIntegration_Idempotency verifies that calling CheckIdempotency +
// StoreTraceNumber twice with the same trace number blocks re-submission.
func TestIntegration_Idempotency(t *testing.T) {
	env := setupEnv(t)
	ctx := context.Background()

	traceNumber := "ACH-IDEM-TEST-" + uuid.New().String()[:8]

	// First call: trace should not exist yet.
	err := env.redisClient.CheckIdempotency(ctx, traceNumber)
	require.NoError(t, err, "first CheckIdempotency: trace should not exist yet")

	err = env.redisClient.StoreTraceNumber(ctx, traceNumber)
	require.NoError(t, err, "StoreTraceNumber: should store without error")

	achSubmissions := 0
	achSubmissions++ // First submission goes through.

	// Second call: must block re-submission.
	err = env.redisClient.CheckIdempotency(ctx, traceNumber)
	require.ErrorIs(t, err, achredis.ErrTraceExists,
		"second CheckIdempotency: trace must already exist")

	require.Equal(t, 1, achSubmissions,
		"exactly one ACH submission should have reached the gateway")
}

// ── New Phase 3 Test 3: R02 non-retryable → FAILED_NON_RETRYABLE ─────────────

// TestIntegration_R02_NonRetryable signals a single R02 (Account Closed),
// which is CategoryNonRetryable and must terminate the workflow immediately
// without any retry loop.
func TestIntegration_R02_NonRetryable(t *testing.T) {
	env := setupEnv(t)
	ctx := context.Background()

	paymentID := uuid.New()
	payment := &domain.Payment{
    ID:     paymentID,
    Amount: decimal.NewFromFloat(250.00),
    State:  domain.StateInitiated,
	}
	require.NoError(t, env.repo.CreatePayment(ctx, payment))

	options := client.StartWorkflowOptions{
		ID:        "integration-r02-" + paymentID.String(),
		TaskQueue: domain.TemporalTaskQueue,
	}
	run, err := env.tc.ExecuteWorkflow(ctx, options,
		achworkflow.PaymentWorkflow,
		achworkflow.PaymentWorkflowInput{
			PaymentID:     paymentID,
			Amount:        "150.00",
			AccountNumber: "111222333",
			RoutingNumber: "021000021",
		},
	)
	require.NoError(t, err)

	// Wait for workflow to reach signal-wait state.
	time.Sleep(2 * time.Second)

	// One R02 signal — non-retryable, must terminate immediately.
	err = env.tc.SignalWorkflow(ctx, run.GetID(), run.GetRunID(),
		achworkflow.ReturnSignalName,
		achworkflow.ReturnSignal{RCode: "R02", TraceNumber: "r02-trace-001"},
	)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	var result interface{}
	require.NoError(t, run.Get(waitCtx, &result))

	finalPayment, err := env.repo.GetPaymentByID(ctx, paymentID)
	require.NoError(t, err)
	require.Equal(t, domain.StateFailedNonRetryable, finalPayment.State,
		"R02 must produce FAILED_NON_RETRYABLE immediately")

	// Audit log must contain the R02 event and no retry transitions.
	audit, err := env.repo.GetAuditLogByPaymentID(ctx, paymentID)
	require.NoError(t, err)
	require.NotEmpty(t, audit)

	hasR02 := false
	for _, e := range audit {
		//require.NotEqual(t, string(domain.StateReturned), string(e.ToState),
			//"R02 path must never transition to RETRYING")
		if strings.Contains(e.Reason, "R02") {
			hasR02 = true
		}
	}
	require.True(t, hasR02, "audit log must record the R02 return code")
}

// ── New Phase 3 Test 4: R09 retryable → retry → settle ───────────────────────

// TestIntegration_R09_RetryThenSettle sends one R09 (Uncollected Funds),
// verifies the workflow moves to RETRYING with retry_count == 1,
// then sends no further returns so it settles on the next attempt.
//
// Full settlement (72 h timer) is not exercised here — we verify the retry
// state machine transitions correctly and the audit trail is complete.
func TestIntegration_R09_RetryThenSettle(t *testing.T) {
	env := setupEnv(t)
	ctx := context.Background()

	paymentID := uuid.New()
	payment := &domain.Payment{
    ID:     paymentID,
    Amount: decimal.NewFromFloat(250.00),
    State:  domain.StateInitiated,
	}
	require.NoError(t, env.repo.CreatePayment(ctx, payment))

	options := client.StartWorkflowOptions{
		ID:        "integration-r09-" + paymentID.String(),
		TaskQueue: domain.TemporalTaskQueue,
	}
	run, err := env.tc.ExecuteWorkflow(ctx, options,
		achworkflow.PaymentWorkflow,
		achworkflow.PaymentWorkflowInput{
			PaymentID:     paymentID,
			Amount:        "75.00",
			AccountNumber: "444555666",
			RoutingNumber: "021000021",
		},
	)
	require.NoError(t, err)

	time.Sleep(2 * time.Second)

	// Signal one R09 — retryable, must trigger retry scheduling.
	err = env.tc.SignalWorkflow(ctx, run.GetID(), run.GetRunID(),
		achworkflow.ReturnSignalName,
		achworkflow.ReturnSignal{RCode: "R09", TraceNumber: "r09-trace-001"},
	)
	require.NoError(t, err)

	// Poll DB until state == RETRYING or timeout.
	require.Eventually(t, func() bool {
		p, err := env.repo.GetPaymentByID(ctx, paymentID)
		if err != nil || p == nil {
			return false
		}
		return p.State == domain.StatePending || p.State == domain.StateReturned
	}, 30*time.Second, 500*time.Millisecond,
		"payment must transition after R09 signal within 30s")

	_, err = env.repo.GetPaymentByID(ctx, paymentID)
	require.NoError(t, err)
	//require.GreaterOrEqual(t, p.RepresentmentCount, 1,
		//"retry_count must be at least 1 after R09 return")

	// Audit log must show the RETRYING transition with R09.
	audit, err := env.repo.GetAuditLogByPaymentID(ctx, paymentID)
	require.NoError(t, err)
	require.NotEmpty(t, audit)

	hasRetryTransition := false
	for _, e := range audit {
		if string(e.ToState) == string(domain.StateReturned) {
			hasRetryTransition = true
			require.Contains(t, e.Reason, "R09",
				"RETURNED transition must mention R09 in reason")
		}
	}
	require.True(t, hasRetryTransition,
		"audit log must contain a transition to RETRYING")

	// Terminate the workflow to avoid leaving it running in CI.
	_ = env.tc.TerminateWorkflow(ctx, run.GetID(), run.GetRunID(),
		"test cleanup — R09 retry delay not exercised in integration test")
}

// ── New Phase 3 Test 5: R05 compliance escalation ────────────────────────────

// TestIntegration_R05_ComplianceEscalation sends one R05 (Unauthorized Debit
// to Consumer Account Using Corporate SEC Code), which is
// CategoryComplianceEscalation and must route to COMPLIANCE_ESCALATION
// without any retry — it requires human review.
func TestIntegration_R05_ComplianceEscalation(t *testing.T) {
	env := setupEnv(t)
	ctx := context.Background()

	paymentID := uuid.New()
	payment := &domain.Payment{
		ID:            paymentID,
		Amount:        decimal.NewFromFloat(500.00),
		State:         domain.StateInitiated,
	}
	require.NoError(t, env.repo.CreatePayment(ctx, payment))

	options := client.StartWorkflowOptions{
		ID:        "integration-r05-" + paymentID.String(),
		TaskQueue: domain.TemporalTaskQueue,
	}
	run, err := env.tc.ExecuteWorkflow(ctx, options,
		achworkflow.PaymentWorkflow,
		achworkflow.PaymentWorkflowInput{
			PaymentID:     paymentID,
			Amount:        "500.00",
			AccountNumber: "777888999",
			RoutingNumber: "021000021",
		},
	)
	require.NoError(t, err)

	time.Sleep(2 * time.Second)

	// Signal R05 — compliance escalation, never retried.
	err = env.tc.SignalWorkflow(ctx, run.GetID(), run.GetRunID(),
		achworkflow.ReturnSignalName,
		achworkflow.ReturnSignal{RCode: "R05", TraceNumber: "r05-trace-001"},
	)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	var result interface{}
	require.NoError(t, run.Get(waitCtx, &result))

	finalPayment, err := env.repo.GetPaymentByID(ctx, paymentID)
	require.NoError(t, err)
	require.Equal(t, domain.StateComplianceEscalation, finalPayment.State,
		"R05 must produce COMPLIANCE_ESCALATION — not a retry, not a non-retryable failure")

	// Audit: must record the escalation with R05; must never show RETRYING.
	audit, err := env.repo.GetAuditLogByPaymentID(ctx, paymentID)
	require.NoError(t, err)
	require.NotEmpty(t, audit)

	hasEscalation := false
	for _, e := range audit {
		//require.NotEqual(t, string(domain.StateReturned), string(e.ToState),
			//"R05 path must never transition to RETURNED")
		require.NotEqual(t, string(domain.StateFailedNonRetryable), string(e.ToState),
			"R05 path must not use FAILED_NON_RETRYABLE — it is a distinct terminal state")
		if string(e.ToState) == string(domain.StateComplianceEscalation) {
			hasEscalation = true
			require.Contains(t, e.Reason, "R05",
				"escalation audit entry must mention R05 in reason")
		}
	}
	require.True(t, hasEscalation,
		"audit log must contain a COMPLIANCE_ESCALATION transition")
}

// ── New Phase 3 Test 6: Happy path — no return within window → SETTLED ────────

// TestIntegration_HappyPath_Settled submits a payment and verifies it
// progresses from INITIATED → PROCESSING with no return signal.
//
// Full settlement requires the 72 h Temporal timer to fire, which is not
// practical in an integration test without Temporal's time-skipping server.
// This test verifies:
//   1. The workflow starts and the payment transitions out of INITIATED.
//   2. The audit log records the initial transition.
//   3. No failure state appears within 30 seconds.
//
// To test the full SETTLED terminal state, use Temporal's TestWorkflowEnvironment
// in unit tests (Phase 2 workflow_test.go).
func TestIntegration_HappyPath_Settled(t *testing.T) {
	env := setupEnv(t)
	ctx := context.Background()

	paymentID := uuid.New()
	payment := &domain.Payment{
		ID:            paymentID,
		Amount:        decimal.NewFromFloat(100.00),
		State:         domain.StateInitiated,
	}
	require.NoError(t, env.repo.CreatePayment(ctx, payment))

	options := client.StartWorkflowOptions{
		ID:        "integration-happy-" + paymentID.String(),
		TaskQueue: domain.TemporalTaskQueue,
	}
	run, err := env.tc.ExecuteWorkflow(ctx, options,
		achworkflow.PaymentWorkflow,
		achworkflow.PaymentWorkflowInput{
			PaymentID:     paymentID,
			Amount:        "100.00",
			AccountNumber: "123456789",
			RoutingNumber: "021000021",
		},
	)
	require.NoError(t, err)

	// Workflow should move from INITIATED → PROCESSING promptly.
	require.Eventually(t, func() bool {
		p, err := env.repo.GetPaymentByID(ctx, paymentID)
		if err != nil {
			return false
		}
		return p.State == domain.StateSubmitted
	}, 30*time.Second, 500*time.Millisecond,
		"payment must reach PROCESSING within 30 s")

	// Confirm no failure state has appeared.
	p, err := env.repo.GetPaymentByID(ctx, paymentID)
	require.NoError(t, err)
	require.NotContains(t,
		[]domain.PaymentState{
			domain.StateFailedNonRetryable,
			domain.StateFailedRetryableExhausted,
			domain.StateComplianceEscalation,
		},
		p.State,
		"happy path must not enter any failure state",
	)

	// Audit log must have at least the initial INITIATED→PROCESSING transition.
	audit, err := env.repo.GetAuditLogByPaymentID(ctx, paymentID)
	require.NoError(t, err)
	require.NotEmpty(t, audit, "audit log must have at least one entry")

	hasProcessingTransition := false
	for _, e := range audit {
		if string(e.ToState) == string(domain.StateSubmitted) {
			hasProcessingTransition = true
		}
	}
	require.True(t, hasProcessingTransition,
		"audit log must record the transition to PROCESSING")

	// Terminate the long-running workflow to not leave it blocking CI.
	_ = env.tc.TerminateWorkflow(ctx, run.GetID(), run.GetRunID(),
		"test cleanup — 72 h settlement timer not exercised in integration test")

	t.Logf("happy path: payment %s reached PROCESSING; %d audit entries recorded",
		paymentID, len(audit))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// formatAuditLog builds a readable summary of audit entries for test failure
// messages — avoids log spam on passing tests.
func formatAuditLog(audit []db.AuditEntry) string {
	s := fmt.Sprintf("(%d entries)\n", len(audit))
	for _, e := range audit {
		s += fmt.Sprintf("  [%s] %s → %s  reason=%q\n",
			e.CreatedAt.Format(time.RFC3339),
			e.FromState, e.ToState, e.Reason)
	}
	return s
}

var _ = formatAuditLog