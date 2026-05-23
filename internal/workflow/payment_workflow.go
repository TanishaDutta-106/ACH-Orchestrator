// Package workflow contains the PaymentWorkflow and its signal/input types.
//
// Temporal workflow rules that apply throughout this file:
//   - Never use goroutines directly — use workflow.Go for coroutines.
//   - Never read wall-clock time — use workflow.Now.
//   - Never sleep with time.Sleep — use workflow.Sleep.
//   - All non-determinism (UUIDs, HTTP calls, DB writes) lives in Activities.
//   - workflow.ExecuteActivity options must set a non-zero StartToCloseTimeout.
package workflow

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/google/uuid"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/activities"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/domain"
)

// ────────────────────────────────────────────────────────────────────────────
// Signal & input types
// ────────────────────────────────────────────────────────────────────────────

// ReturnSignalName is the Temporal signal name the workflow listens on.
// External callers (the Phase 3 REST handler) send this signal when NACHA
// delivers an R-code notification.
const ReturnSignalName = "ReturnSignal"

// ReturnSignal carries the NACHA return code and optional trace number from
// the ACH network back into the workflow.
type ReturnSignal struct {
	RCode       string // e.g. "R01", "R09"
	TraceNumber string // echoed back for audit purposes
}

// PaymentWorkflowInput is the argument to PaymentWorkflow.
// All fields must be JSON-serialisable (Temporal default codec).
type PaymentWorkflowInput struct {
	PaymentID     uuid.UUID
	Amount        string // decimal string
	AccountNumber string
	RoutingNumber string
}

// ────────────────────────────────────────────────────────────────────────────
// Default activity options
// ────────────────────────────────────────────────────────────────────────────

// defaultActivityOptions returns sane defaults for all activity calls in this
// workflow.  Individual call sites may override ScheduleToCloseTimeout for
// long-running or time-sensitive steps.
func defaultActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		// Temporal requires at least one of Start/Schedule timeout.
		StartToCloseTimeout: 30 * time.Second,
		// Retry non-application errors (transient DB/Redis/network blips) up to
		// 3 times with exponential backoff.
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
}

// ────────────────────────────────────────────────────────────────────────────
// PaymentWorkflow
// ────────────────────────────────────────────────────────────────────────────

// PaymentWorkflow is the top-level Temporal workflow for ACH payment processing.
//
// Lifecycle (matches the Phase 2 spec exactly):
//
//  1. Transition INITIATED → PENDING.
//  2. Generate a trace number, check Redis idempotency, store it, submit to ACH.
//  3. Wait for a ReturnSignal OR a 72-hour settlement timer — whichever fires first.
//  4. If the timer fires first → SETTLED (payment cleared, no dispute).
//  5. If a ReturnSignal arrives → route the R-code:
//     a. NonRetryable        → FAILED_NON_RETRYABLE, done.
//     b. ComplianceEscalation → COMPLIANCE_ESCALATION, done.
//     c. Retryable, attempts < MaxRepresentments → sleep RetryDelayFor(rCode),
//     increment representment counter, new trace number, loop to step 2.
//     d. Retryable, attempts >= MaxRepresentments → FAILED_RETRYABLE_EXHAUSTED.
func PaymentWorkflow(ctx workflow.Context, input PaymentWorkflowInput) error {
	logger := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions())

	// ── Step 1: INITIATED → PENDING ─────────────────────────────────────────
	if err := persistTransition(ctx, input.PaymentID,
		domain.StateInitiated, domain.StatePending,
		"workflow started"); err != nil {
		return err
	}
	logger.Info("PaymentWorkflow: transitioned to PENDING", "payment_id", input.PaymentID)

	// ── Retry loop ───────────────────────────────────────────────────────────
	representmentCount := 0

	for {
		// ── Step 2: Generate trace, check idempotency, store, submit ─────────

		// GenerateTraceNumber is pure (no side effects) so it can live in the
		// workflow directly.  Each loop iteration produces a new unique trace.
		traceNumber := generateTraceNumber(ctx, representmentCount, input.PaymentID)

		// Check idempotency — if this trace was already submitted (e.g. workflow
		// replaying after a crash mid-activity), skip submission.
		checkOut, err := checkIdempotency(ctx, traceNumber)
		if err != nil {
			return err
		}

		if !checkOut.AlreadySubmitted {
			// Store the trace number before submitting so that if the worker
			// crashes between store and submit, the next replay skips submission.
			if err := storeTraceNumber(ctx, traceNumber); err != nil {
				return err
			}

			// Transition PENDING → SUBMITTED before sending to ACH so the DB
			// always reflects the most recent intent.
			if err := persistTransition(ctx, input.PaymentID,
				domain.StatePending, domain.StateSubmitted,
				fmt.Sprintf("submitting to ACH, trace=%s, representment=%d",
					traceNumber, representmentCount)); err != nil {
				return err
			}

			if _, err := submitToACH(ctx, input, traceNumber); err != nil {
				return err
			}
			logger.Info("PaymentWorkflow: submitted to ACH",
				"payment_id", input.PaymentID,
				"trace_number", traceNumber,
				"representment", representmentCount,
			)
		} else {
			logger.Info("PaymentWorkflow: idempotency hit — skipping ACH submission",
				"payment_id", input.PaymentID,
				"trace_number", traceNumber,
			)
		}

		// ── Step 3: Race between ReturnSignal and 72-hour settlement timer ────

		// settlementTimer fires if no return is received within 72 hours.
		// Per spec this is a fixed value — do not parameterise.
		settlementTimer := workflow.NewTimer(ctx, 72*time.Hour)

		returnCh := workflow.GetSignalChannel(ctx, ReturnSignalName)

		// selector blocks until one of the two cases fires.
		var returnSig ReturnSignal
		returnReceived := false

		sel := workflow.NewSelector(ctx)

		sel.AddFuture(settlementTimer, func(f workflow.Future) {
			// Timer fired — no return received in 72 hours → SETTLED.
			_ = f.Get(ctx, nil)
		})

		sel.AddReceive(returnCh, func(ch workflow.ReceiveChannel, more bool) {
			ch.Receive(ctx, &returnSig)
			returnReceived = true
		})

		sel.Select(ctx)

		// ── Step 4 / 5: Branch on what fired ─────────────────────────────────

		if !returnReceived {
			// ── Happy path: settlement timer fired ───────────────────────────
			logger.Info("PaymentWorkflow: 72h window elapsed, settling",
				"payment_id", input.PaymentID,
			)
			return persistTransition(ctx, input.PaymentID,
				domain.StateSubmitted, domain.StateSettled,
				"settled — no return within 72h")
		}

		// ── Return signal received — route the R-code ─────────────────────────
		category, description, _ := domain.RouteRCode(returnSig.RCode)
		logger.Info("PaymentWorkflow: return received",
			"payment_id", input.PaymentID,
			"rcode", returnSig.RCode,
			"category", category,
			"description", description,
		)

		// Persist SUBMITTED → RETURNED before branching so the DB always
		// reflects that a return was received, regardless of what happens next.
		if err := persistTransition(ctx, input.PaymentID,
			domain.StateSubmitted, domain.StateReturned,
			fmt.Sprintf("R-code %s received: %s", returnSig.RCode, description)); err != nil {
			return err
		}

		switch category {
		case domain.CategoryNonRetryable:
			// ── Non-retryable: terminate ──────────────────────────────────────
			logger.Info("PaymentWorkflow: non-retryable return, failing",
				"payment_id", input.PaymentID,
				"rcode", returnSig.RCode,
			)
			return persistTransition(ctx, input.PaymentID,
				domain.StateReturned, domain.StateFailedNonRetryable,
				fmt.Sprintf("non-retryable R-code %s: %s", returnSig.RCode, description))

		case domain.CategoryComplianceEscalation:
			// ── Compliance escalation: terminate ─────────────────────────────
			logger.Info("PaymentWorkflow: compliance escalation",
				"payment_id", input.PaymentID,
				"rcode", returnSig.RCode,
			)
			return persistTransition(ctx, input.PaymentID,
				domain.StateReturned, domain.StateComplianceEscalation,
				fmt.Sprintf("compliance escalation R-code %s: %s", returnSig.RCode, description))

		case domain.CategoryRetryable:
			if representmentCount >= domain.MaxRepresentments {
				// ── Exhausted representments ──────────────────────────────────
				logger.Info("PaymentWorkflow: retryable exhausted",
					"payment_id", input.PaymentID,
					"rcode", returnSig.RCode,
					"representment_count", representmentCount,
				)
				return persistTransition(ctx, input.PaymentID,
					domain.StateReturned, domain.StateFailedRetryableExhausted,
					fmt.Sprintf("retryable R-code %s exhausted after %d representments",
						returnSig.RCode, representmentCount))
			}

			// ── Schedule retry: sleep, then loop ─────────────────────────────
			delay := domain.RetryDelayFor(returnSig.RCode)
			logger.Info("PaymentWorkflow: scheduling retry",
				"payment_id", input.PaymentID,
				"rcode", returnSig.RCode,
				"delay", delay,
				"representment_after_sleep", representmentCount+1,
			)

			// Transition back to PENDING before sleeping so the DB reflects
			// that we intend to retry.
			if err := persistTransition(ctx, input.PaymentID,
				domain.StateReturned, domain.StatePending,
				fmt.Sprintf("retrying after R-code %s, sleeping %s", returnSig.RCode, delay)); err != nil {
				return err
			}

			// Use workflow.Sleep — never time.Sleep inside a workflow.
			if err := workflow.Sleep(ctx, delay); err != nil {
				return err
			}

			representmentCount++
			// Loop back to the top — new trace number, new ACH submission.
			continue

		default:
			// Unknown category — treat as NonRetryable (safe default from Phase 1).
			return persistTransition(ctx, input.PaymentID,
				domain.StateReturned, domain.StateFailedNonRetryable,
				fmt.Sprintf("unknown R-code %s treated as non-retryable", returnSig.RCode))
		}
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Private helpers — thin wrappers that keep the main workflow readable
// ────────────────────────────────────────────────────────────────────────────

func persistTransition(
	ctx workflow.Context,
	paymentID uuid.UUID,
	from, to domain.PaymentState,
	reason string,
) error {
	return workflow.ExecuteActivity(ctx,
		(*activities.Activities).PersistStateTransition,
		activities.PersistStateTransitionInput{
			PaymentID: paymentID,
			FromState: from,
			ToState:   to,
			Reason:    reason,
		},
	).Get(ctx, nil)
}

func checkIdempotency(ctx workflow.Context, traceNumber string) (activities.CheckIdempotencyOutput, error) {
	var out activities.CheckIdempotencyOutput
	err := workflow.ExecuteActivity(ctx,
		(*activities.Activities).CheckIdempotency,
		activities.CheckIdempotencyInput{TraceNumber: traceNumber},
	).Get(ctx, &out)
	return out, err
}

func storeTraceNumber(ctx workflow.Context, traceNumber string) error {
	return workflow.ExecuteActivity(ctx,
		(*activities.Activities).StoreTraceNumber,
		activities.StoreTraceNumberInput{TraceNumber: traceNumber},
	).Get(ctx, nil)
}

func submitToACH(
	ctx workflow.Context,
	input PaymentWorkflowInput,
	traceNumber string,
) (activities.SubmitToACHOutput, error) {
	var out activities.SubmitToACHOutput
	err := workflow.ExecuteActivity(ctx,
		(*activities.Activities).SubmitToACH,
		activities.SubmitToACHInput{
			PaymentID:     input.PaymentID,
			Amount:        input.Amount,
			AccountNumber: input.AccountNumber,
			RoutingNumber: input.RoutingNumber,
			TraceNumber:   traceNumber,
		},
	).Get(ctx, &out)
	return out, err
}

// generateTraceNumber produces a deterministic-per-attempt trace number.
//
// It is called inside the workflow (not an activity) because it is pure:
// given the same paymentID and representmentCount it always produces the same
// value on workflow replay, satisfying Temporal's determinism requirement.
// We embed the representment count so retries never collide with the original.
func generateTraceNumber(ctx workflow.Context, representmentCount int, paymentID uuid.UUID) string {
	// Use workflow.Now for the timestamp component so the value is replay-safe.
	ts := workflow.Now(ctx).UTC().Unix()
	return fmt.Sprintf("ACH-%s-r%d-%d", paymentID.String()[:8], representmentCount, ts)
}
