// Package simulator generates NACHA-formatted ACH return files for a given
// payment and R-code, verifies they parse correctly (round-trip), then
// signals the running PaymentWorkflow via Temporal's ReturnSignal.
package simulator

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/nacha"
	achworkflow "github.com/TanishaDutta-106/ACH-Orchestrator/internal/workflow"
)

// ReturnSimulator wraps a Temporal client to send simulated return events
// to running PaymentWorkflow instances.
type ReturnSimulator struct {
	temporalClient client.Client
}

// New creates a ReturnSimulator backed by the provided Temporal client.
func New(c client.Client) *ReturnSimulator {
	return &ReturnSimulator{temporalClient: c}
}

// SimulateReturn does three things:
//  1. Generates a valid NACHA return file string from the supplied fields.
//  2. Parses it back to verify round-trip integrity.
//  3. Signals the running PaymentWorkflow for paymentID with ReturnSignal.
//
// workflowID must match the ID used when ExecuteWorkflow was called.
// Per the project convention that ID == paymentID.String().
func (s *ReturnSimulator) SimulateReturn(
	ctx context.Context,
	paymentID uuid.UUID,
	routingNumber string,
	accountNumber string,
	amountCents int64,
	rcode string,
) error {
	traceNumber := buildTraceNumber(routingNumber, paymentID)

	// ── Step 1: Generate NACHA file ───────────────────────────────────────────
	fileContent := nacha.GenerateReturnFile(
		routingNumber,
		accountNumber,
		amountCents,
		traceNumber,
		rcode,
	)

	// ── Step 2: Parse and verify round-trip ───────────────────────────────────
	entries, err := nacha.ParseFile(fileContent)
	if err != nil {
		return fmt.Errorf("simulator: generated file failed to parse: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("simulator: generated file contained no entries")
	}
	if entries[0].RCode != rcode {
		return fmt.Errorf("simulator: round-trip R-code mismatch: want %q got %q",
			rcode, entries[0].RCode)
	}
	if entries[0].Amount != amountCents {
		return fmt.Errorf("simulator: round-trip amount mismatch: want %d got %d",
			amountCents, entries[0].Amount)
	}

	// ── Step 3: Signal the Temporal workflow ──────────────────────────────────
	// workflowID == paymentID.String() — set in cmd/server/main.go at
	// ExecuteWorkflow time. runID is left empty to target the latest run.
	return s.temporalClient.SignalWorkflow(
		ctx,
		paymentID.String(), // workflow ID
		"",                 // runID: latest run
		achworkflow.ReturnSignalName,
		achworkflow.ReturnSignal{
			RCode:       rcode,
			TraceNumber: traceNumber,
		},
	)
}

// SimulateReturnFromFile parses an existing NACHA file and signals every
// return entry whose trace number maps to a known payment.
// paymentIDByTrace maps traceNumber → paymentID.
func (s *ReturnSimulator) SimulateReturnFromFile(
	ctx context.Context,
	fileContent string,
	paymentIDByTrace map[string]uuid.UUID,
) error {
	entries, err := nacha.ParseFile(fileContent)
	if err != nil {
		return fmt.Errorf("simulator: failed to parse return file: %w", err)
	}

	for _, entry := range entries {
		paymentID, ok := paymentIDByTrace[entry.TraceNumber]
		if !ok {
			continue // Unknown trace number — skip.
		}
		if err := s.temporalClient.SignalWorkflow(
			ctx,
			paymentID.String(),
			"",
			achworkflow.ReturnSignalName,
			achworkflow.ReturnSignal{
				RCode:       entry.RCode,
				TraceNumber: entry.TraceNumber,
			},
		); err != nil {
			return fmt.Errorf("simulator: signal failed for payment %s (trace %s): %w",
				paymentID, entry.TraceNumber, err)
		}
	}
	return nil
}

// buildTraceNumber constructs a 15-digit NACHA trace number.
// Format: [8-digit ODFI routing][7-digit sequence derived from paymentID].
func buildTraceNumber(routingNumber string, paymentID uuid.UUID) string {
	// Extract digits from the UUID to build the 7-digit sequence.
	digits := ""
	for _, c := range paymentID.String() {
		if c >= '0' && c <= '9' {
			digits += string(c)
			if len(digits) == 7 {
				break
			}
		}
	}
	// If the UUID had fewer than 7 digits (very unlikely), pad with timestamp.
	for len(digits) < 7 {
		ts := fmt.Sprintf("%d", time.Now().UnixNano())
		digits = ts[len(ts)-7:]
	}

	odfi := routingNumber
	if len(odfi) > 8 {
		odfi = odfi[:8]
	}
	for len(odfi) < 8 {
		odfi = "0" + odfi
	}

	return odfi + digits
}
