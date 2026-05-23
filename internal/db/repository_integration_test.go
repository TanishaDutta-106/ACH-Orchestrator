//go:build integration
// +build integration

// Integration tests for the repository layer. These tests require a real
// PostgreSQL instance. Run with:
//
//	DATABASE_URL="postgres://ach_user:ach_secret@localhost:5432/ach_orchestrator?sslmode=disable" \
//	  go test ./internal/db/... -v -tags integration
//
// The docker-compose.yml in the project root starts a suitable instance.
package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/db"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/domain"
)

// newTestRepo creates a Repository connected to the DATABASE_URL environment
// variable. Skips the test if the variable is not set.
func newTestRepo(t *testing.T) *db.Repository {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	repo, err := db.NewRepository(ctx, dsn)
	if err != nil {
		t.Fatalf("newTestRepo: %v", err)
	}
	t.Cleanup(repo.Close)
	return repo
}

// TestIntegration_CreateAndGetPayment is the primary integration test.
// It exercises the full happy path:
//
//	CreatePayment → UpdatePaymentState (INITIATED→PENDING) →
//	UpdatePaymentState (PENDING→SUBMITTED) → GetPaymentByID →
//	GetAuditLogByPaymentID
func TestIntegration_CreateAndGetPayment(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	// ── 1. Create a payment ──────────────────────────────────────────────────
	portfolioID := uuid.New()
	amount, _ := decimal.NewFromString("1234.56")

	p := &domain.Payment{
		PortfolioID:        portfolioID,
		Amount:             amount,
		State:              domain.StateInitiated,
		TraceNumber:        "123456789012345",
		RepresentmentCount: 0,
	}

	if err := repo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}

	if p.ID == uuid.Nil {
		t.Fatal("CreatePayment: expected non-nil ID after insert")
	}
	if p.CreatedAt.IsZero() {
		t.Fatal("CreatePayment: expected CreatedAt to be set")
	}

	t.Logf("created payment ID: %s", p.ID)

	// ── 2. Transition INITIATED → PENDING ────────────────────────────────────
	if err := repo.UpdatePaymentState(ctx, p, domain.StatePending, "queued for submission"); err != nil {
		t.Fatalf("UpdatePaymentState INITIATED→PENDING: %v", err)
	}
	if p.State != domain.StatePending {
		t.Errorf("expected state PENDING, got %s", p.State)
	}

	// ── 3. Transition PENDING → SUBMITTED ────────────────────────────────────
	if err := repo.UpdatePaymentState(ctx, p, domain.StateSubmitted, "ACH file submitted to ODFI"); err != nil {
		t.Fatalf("UpdatePaymentState PENDING→SUBMITTED: %v", err)
	}
	if p.State != domain.StateSubmitted {
		t.Errorf("expected state SUBMITTED, got %s", p.State)
	}

	// ── 4. Read back and verify ───────────────────────────────────────────────
	fetched, err := repo.GetPaymentByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPaymentByID: %v", err)
	}
	if fetched == nil {
		t.Fatal("GetPaymentByID: returned nil, expected a payment")
	}

	if fetched.ID != p.ID {
		t.Errorf("ID mismatch: got %s, want %s", fetched.ID, p.ID)
	}
	if fetched.State != domain.StateSubmitted {
		t.Errorf("state mismatch: got %s, want SUBMITTED", fetched.State)
	}
	if !fetched.Amount.Equal(amount) {
		t.Errorf("amount mismatch: got %s, want %s", fetched.Amount, amount)
	}
	if fetched.PortfolioID != portfolioID {
		t.Errorf("portfolio_id mismatch")
	}
	if fetched.TraceNumber != "123456789012345" {
		t.Errorf("trace_number mismatch: got %q", fetched.TraceNumber)
	}

	// ── 5. Check audit log has two entries ───────────────────────────────────
	entries, err := repo.GetAuditLogByPaymentID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetAuditLogByPaymentID: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 audit entries, got %d", len(entries))
	}

	// Entries are ordered chronologically (oldest first).
	if entries[0].FromState != domain.StateInitiated || entries[0].ToState != domain.StatePending {
		t.Errorf("audit[0]: expected INITIATED→PENDING, got %s→%s", entries[0].FromState, entries[0].ToState)
	}
	if entries[1].FromState != domain.StatePending || entries[1].ToState != domain.StateSubmitted {
		t.Errorf("audit[1]: expected PENDING→SUBMITTED, got %s→%s", entries[1].FromState, entries[1].ToState)
	}
}

// TestIntegration_ReturnEventAndSettlement exercises the return event path
// and then verifies settled_at is set correctly.
func TestIntegration_ReturnEventAndSettlement(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	// Create and advance to SUBMITTED.
	amount, _ := decimal.NewFromString("500.00")
	p := &domain.Payment{
		PortfolioID: uuid.New(),
		Amount:      amount,
		State:       domain.StateInitiated,
		TraceNumber: "999888777666555",
	}
	if err := repo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if err := repo.UpdatePaymentState(ctx, p, domain.StatePending, "test setup"); err != nil {
		t.Fatalf("UpdatePaymentState: %v", err)
	}
	if err := repo.UpdatePaymentState(ctx, p, domain.StateSubmitted, "test setup"); err != nil {
		t.Fatalf("UpdatePaymentState: %v", err)
	}

	// Simulate receiving an R01 return.
	ev := &db.ReturnEvent{
		PaymentID:    p.ID,
		RCode:        "R01",
		RawNACHALine: "6220000000001234567890123456789INSUFFICIENT FUNDS            0123456789012345",
	}
	if err := repo.InsertReturnEvent(ctx, ev); err != nil {
		t.Fatalf("InsertReturnEvent: %v", err)
	}
	if ev.ID == uuid.Nil {
		t.Error("InsertReturnEvent: expected non-nil ID")
	}

	// Transition to RETURNED.
	if err := repo.UpdatePaymentState(ctx, p, domain.StateReturned, "R01 received"); err != nil {
		t.Fatalf("UpdatePaymentState SUBMITTED→RETURNED: %v", err)
	}

	// Simulate a successful representment → SETTLED.
	if err := repo.UpdatePaymentState(ctx, p, domain.StatePending, "representment 1"); err != nil {
		t.Fatalf("UpdatePaymentState RETURNED→PENDING: %v", err)
	}
	if err := repo.UpdatePaymentState(ctx, p, domain.StateSubmitted, "re-submitted"); err != nil {
		t.Fatalf("UpdatePaymentState PENDING→SUBMITTED: %v", err)
	}
	if err := repo.UpdatePaymentState(ctx, p, domain.StateSettled, "funds confirmed"); err != nil {
		t.Fatalf("UpdatePaymentState SUBMITTED→SETTLED: %v", err)
	}

	// Verify settled_at is populated.
	settled, err := repo.GetPaymentByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPaymentByID: %v", err)
	}
	if settled.SettledAt == nil {
		t.Error("expected settled_at to be set after SETTLED transition")
	}
	if settled.FailedAt != nil {
		t.Error("expected failed_at to remain nil for a settled payment")
	}
}

// TestIntegration_IllegalTransitionRejected verifies that the repository
// enforces FSM rules and refuses an illegal state transition.
func TestIntegration_IllegalTransitionRejected(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	amount, _ := decimal.NewFromString("100.00")
	p := &domain.Payment{
		PortfolioID: uuid.New(),
		Amount:      amount,
		State:       domain.StateInitiated,
		TraceNumber: "111222333444555",
	}
	if err := repo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}

	// Trying to jump from INITIATED directly to SETTLED must fail.
	err := repo.UpdatePaymentState(ctx, p, domain.StateSettled, "illegal jump")
	if err == nil {
		t.Fatal("expected error for illegal INITIATED→SETTLED transition, got nil")
	}
	t.Logf("correctly rejected illegal transition: %v", err)

	// Payment state on the struct must not have changed.
	if p.State != domain.StateInitiated {
		t.Errorf("struct state should remain INITIATED after rejected transition, got %s", p.State)
	}
}

// TestIntegration_GetPaymentByID_NotFound verifies that querying a non-existent
// ID returns (nil, nil) — no error, just nil payment.
func TestIntegration_GetPaymentByID_NotFound(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	nonExistent := uuid.New()
	p, err := repo.GetPaymentByID(ctx, nonExistent)
	if err != nil {
		t.Fatalf("GetPaymentByID for non-existent ID returned error: %v", err)
	}
	if p != nil {
		t.Errorf("expected nil payment for non-existent ID, got %+v", p)
	}
}

// TestIntegration_AuditLogOrdering verifies that audit entries come back in
// chronological order (oldest first) regardless of insertion order.
func TestIntegration_AuditLogOrdering(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	amount, _ := decimal.NewFromString("250.00")
	p := &domain.Payment{
		PortfolioID: uuid.New(),
		Amount:      amount,
		State:       domain.StateInitiated,
		TraceNumber: "555444333222111",
	}
	if err := repo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}

	transitions := []struct {
		to     domain.PaymentState
		reason string
	}{
		{domain.StatePending, "step 1"},
		{domain.StateSubmitted, "step 2"},
		{domain.StateReturned, "step 3 - R09 received"},
		{domain.StatePending, "step 4 - representment"},
		{domain.StateSubmitted, "step 5 - re-submitted"},
		{domain.StateSettled, "step 6 - settled"},
	}

	for _, tr := range transitions {
		if err := repo.UpdatePaymentState(ctx, p, tr.to, tr.reason); err != nil {
			t.Fatalf("transition to %s: %v", tr.to, err)
		}
	}

	entries, err := repo.GetAuditLogByPaymentID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetAuditLogByPaymentID: %v", err)
	}
	if len(entries) != len(transitions) {
		t.Fatalf("expected %d audit entries, got %d", len(transitions), len(entries))
	}

	// Verify chronological ordering by checking created_at strictly increases.
	for i := 1; i < len(entries); i++ {
		if !entries[i].CreatedAt.After(entries[i-1].CreatedAt) &&
			!entries[i].CreatedAt.Equal(entries[i-1].CreatedAt) {
			t.Errorf(
				"audit entry %d is not after entry %d: %v vs %v",
				i, i-1, entries[i].CreatedAt, entries[i-1].CreatedAt,
			)
		}
	}

	// Verify reason strings are preserved in order.
	for i, tr := range transitions {
		if entries[i].Reason != tr.reason {
			t.Errorf("entry %d reason: got %q, want %q", i, entries[i].Reason, tr.reason)
		}
	}
}
