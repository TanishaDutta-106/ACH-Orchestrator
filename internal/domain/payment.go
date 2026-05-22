// Package domain contains the core business entities for the ACH Payment
// Retry Orchestrator. These types are the source of truth for payment
// lifecycle state — they are shared across the repository, workflow, and
// API layers (added in later phases).
package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// PaymentState represents every possible lifecycle state a payment can occupy.
// Transitions between states are governed by NACHA rules and the retry policy
// defined in rules.go. Never store raw integers; always use the named constants
// so that log output and database rows are human-readable.
type PaymentState string

const (
	// StateInitiated is the entry point. The payment record has been created
	// but has not yet been submitted to the ACH network.
	StateInitiated PaymentState = "INITIATED"

	// StatePending means the payment file has been assembled and is awaiting
	// submission to the ODFI (Originating Depository Financial Institution).
	StatePending PaymentState = "PENDING"

	// StateSubmitted means the ACH entry has been sent to the ODFI and is
	// in-flight on the network. We are waiting for a settlement or return.
	StateSubmitted PaymentState = "SUBMITTED"

	// StateSettled means funds have been confirmed as transferred. This is a
	// terminal success state.
	StateSettled PaymentState = "SETTLED"

	// StateFailedNonRetryable means the payment returned an R-code that
	// categorically prohibits retrying (e.g. R03 – No Account / Unable to
	// Locate Account). Retrying would violate NACHA rules. Terminal state.
	StateFailedNonRetryable PaymentState = "FAILED_NON_RETRYABLE"

	// StateFailedRetryableExhausted means the payment was retryable (e.g. R01
	// – Insufficient Funds) but has consumed all allowed representment attempts
	// (maximum 2 per NACHA rules). Terminal state.
	StateFailedRetryableExhausted PaymentState = "FAILED_RETRYABLE_EXHAUSTED"

	// StateComplianceEscalation means the return code indicates a potential
	// legal or regulatory issue (e.g. R05 – Unauthorized Debit). The payment
	// must be reviewed by a human compliance officer before any further action.
	// Terminal state from the automation perspective.
	StateComplianceEscalation PaymentState = "COMPLIANCE_ESCALATION"

	// StateReturned means an ACH return entry (R-code) was received from the
	// RDFI. This is a transient state: the R-code router then drives the
	// payment into Retryable, NonRetryable, or ComplianceEscalation.
	StateReturned PaymentState = "RETURNED"
)

// Payment is the central aggregate for the ACH retry system. One Payment
// record tracks a single debit or credit entry through its entire lifecycle,
// including all retry attempts.
//
// Monetary amounts use shopspring/decimal to avoid floating-point rounding
// errors — critical for financial data. Never use float32/float64 for money.
type Payment struct {
	// ID is the system-generated primary key. UUIDs are used so that IDs are
	// safe to expose externally without leaking sequence information.
	ID uuid.UUID `db:"id"`

	// PortfolioID groups payments belonging to the same originating business
	// or logical account portfolio. Used for filtering and reporting.
	PortfolioID uuid.UUID `db:"portfolio_id"`

	// Amount is the payment value in USD. Stored as NUMERIC(19,4) in Postgres.
	// shopspring/decimal handles serialization/deserialization cleanly with pgx.
	Amount decimal.Decimal `db:"amount"`

	// State is the current lifecycle position of this payment.
	// See PaymentState constants above for valid values and semantics.
	State PaymentState `db:"state"`

	// ReturnCode is the NACHA R-code received from the RDFI, e.g. "R01".
	// Empty string when no return has been received yet.
	ReturnCode string `db:"return_code"`

	// RepresentmentCount tracks how many times this payment has been
	// re-submitted after an initial return. NACHA allows a maximum of 2
	// representments for eligible return codes. See MaxRepresentments in
	// rules.go.
	RepresentmentCount int `db:"representment_count"`

	// TraceNumber is the 15-digit ACH trace number assigned by the ODFI.
	// It uniquely identifies an ACH entry within a file. Stored as a string
	// because leading zeros are significant and numeric types would drop them.
	TraceNumber string `db:"trace_number"`

	// CreatedAt is the UTC timestamp when this payment record was first
	// inserted. Set once at creation; never updated.
	CreatedAt time.Time `db:"created_at"`

	// UpdatedAt is refreshed on every state transition. Useful for detecting
	// stale records and for ordering queries by recency.
	UpdatedAt time.Time `db:"updated_at"`

	// SettledAt is non-nil only when the payment reaches StateSettled.
	// Nil for all other states.
	SettledAt *time.Time `db:"settled_at"`

	// FailedAt is non-nil only when the payment reaches a terminal failure
	// state (StateFailedNonRetryable, StateFailedRetryableExhausted, or
	// StateComplianceEscalation). Nil for all other states.
	FailedAt *time.Time `db:"failed_at"`
}
