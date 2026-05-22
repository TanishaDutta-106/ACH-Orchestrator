// Package domain — rules.go
//
// NACHA business rules and retry policy constants. These values are derived
// from the NACHA Operating Rules and should not be changed without confirming
// the current rule version. Every constant has a citation comment.
package domain

import "time"

// ── Representment limits ──────────────────────────────────────────────────────

// MaxRepresentments is the maximum number of times a returned ACH entry may be
// re-presented to the RDFI for eligible R-codes.
//
// NACHA Rule: An originator may re-initiate a returned entry no more than two
// times following the return of the original entry (NACHA Operating Rules,
// Subsection 2.12.4). Counting starts at 0 for the original attempt, so
// RepresentmentCount == 2 means the payment has used all allowed retries.
const MaxRepresentments = 2

// ── Retry timing windows ──────────────────────────────────────────────────────

// RetryDelayR01 is the recommended minimum delay before re-presenting an R01
// (Insufficient Funds) return. NACHA does not mandate an exact delay, but
// industry practice is 24–72 hours. We use 24 hours as the floor.
const RetryDelayR01 = 24 * time.Hour

// RetryDelayR09 is the recommended minimum delay before re-presenting an R09
// (Uncollected Funds) return. Deposited funds typically clear within 2 business
// days, so a 48-hour delay is a reasonable default.
const RetryDelayR09 = 48 * time.Hour

// RetryDelayR08 is the recommended minimum delay before re-presenting an R08
// (Payment Stopped) return. A stop-payment order has a finite life at most
// banks (often 6 months). Re-presenting after 24 hours gives the originator
// time to contact the customer before the next attempt.
const RetryDelayR08 = 24 * time.Hour

// RetryDelayDefault is used for any retryable R-code that does not have a
// specific delay defined above.
const RetryDelayDefault = 24 * time.Hour

// ── Settlement timing ─────────────────────────────────────────────────────────

// ACHSettlementWindow is the standard ACH settlement timeline.
// Under same-day ACH, credit entries settle within hours; debit entries
// typically settle the next business day. This constant represents the
// worst-case standard debit settlement window for timeout/staleness detection.
const ACHSettlementWindow = 2 * 24 * time.Hour

// ReturnDeadline is the maximum time an RDFI has to return an ACH entry.
// Standard return window: 2 banking days from the settlement date for most
// return reasons (NACHA Operating Rules, Subsection 2.12.2). Extended returns
// (e.g. R07, R10) allow up to 60 calendar days, but our orchestrator treats
// anything beyond this standard window as potentially stale.
const ReturnDeadline = 2 * 24 * time.Hour

// ── Valid state transitions ───────────────────────────────────────────────────

// AllowedTransitions defines which state transitions are valid. Any transition
// not listed here is illegal and must be rejected by the repository layer to
// prevent data corruption.
//
// Design note: using a map of sets (represented as map[PaymentState]bool) keeps
// the transition logic data-driven and easy to audit. Add new transitions here
// before writing code that performs them.
var AllowedTransitions = map[PaymentState]map[PaymentState]bool{
	StateInitiated: {
		StatePending: true,
	},
	StatePending: {
		StateSubmitted: true,
		// A payment can be cancelled from Pending before submission.
		StateFailedNonRetryable: true,
	},
	StateSubmitted: {
		StateSettled:              true,
		StateReturned:             true,
		StateFailedNonRetryable:   true, // e.g. ODFI-initiated return
		StateComplianceEscalation: true,
	},
	StateReturned: {
		// After routing the R-code, the orchestrator transitions the payment
		// to one of these states.
		StatePending:                   true, // re-presentment queued
		StateFailedNonRetryable:        true,
		StateFailedRetryableExhausted:  true,
		StateComplianceEscalation:      true,
	},
	// Terminal states — no outbound transitions permitted.
	StateSettled:                  {},
	StateFailedNonRetryable:       {},
	StateFailedRetryableExhausted: {},
	StateComplianceEscalation:     {},
}

// IsTransitionAllowed returns true if transitioning from `from` to `to` is a
// valid state change per NACHA business rules and our orchestrator's FSM.
func IsTransitionAllowed(from, to PaymentState) bool {
	targets, ok := AllowedTransitions[from]
	if !ok {
		return false
	}
	return targets[to]
}

// IsTerminalState returns true if the payment has reached a state from which
// no further automated transitions are possible. Used by the Temporal workflow
// (Phase 2) to decide whether to stop scheduling activities.
func IsTerminalState(state PaymentState) bool {
	targets, ok := AllowedTransitions[state]
	return ok && len(targets) == 0
}

// RetryDelayFor returns the recommended delay before re-presenting a payment
// that returned with the given R-code. Returns RetryDelayDefault for any
// retryable code that doesn't have a specific delay configured.
//
// Callers should only invoke this for CategoryRetryable codes; calling it for
// NonRetryable or ComplianceEscalation codes is a logic error and will return
// RetryDelayDefault (but the payment should not actually be retried).
func RetryDelayFor(rCode string) time.Duration {
	switch rCode {
	case "R01":
		return RetryDelayR01
	case "R08":
		return RetryDelayR08
	case "R09":
		return RetryDelayR09
	default:
		return RetryDelayDefault
	}
}

// ── Temporal task queue ───────────────────────────────────────────────────────

// TemporalTaskQueue is the Temporal task queue name used throughout the entire
// project. Defined here (not in the workflow layer) so that both the worker
// and the API can import it from a single source of truth without creating an
// import cycle.
//
// Phase 2 note: import this constant instead of hardcoding the string.
const TemporalTaskQueue = "ach-payment-queue"
