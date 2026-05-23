// Package db implements the PostgreSQL persistence layer for the ACH Payment
// Retry Orchestrator. All database operations live here. Business logic does
// not belong in this package — it belongs in the domain or workflow layers.
//
// Driver: pgx/v5 (jackc/pgx). We use the pgxpool connection pool for
// production-safe concurrent access. Do not use database/sql — pgx/v5's
// native interface is more ergonomic and avoids unnecessary reflection.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/domain"
)

// Repository holds the pgxpool connection pool and exposes all database
// operations as methods. Create one instance at startup and share it across
// goroutines — pgxpool is safe for concurrent use.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository opens a connection pool to the PostgreSQL database at the
// given DSN and verifies connectivity with a Ping.
//
// Example DSN: "postgres://user:pass@localhost:5432/ach_orchestrator?sslmode=disable"
func NewRepository(ctx context.Context, dsn string) (*Repository, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	return &Repository{pool: pool}, nil
}

// Close releases all connections in the pool. Call this during graceful shutdown.
func (r *Repository) Close() {
	r.pool.Close()
}

// ── ReturnEvent ───────────────────────────────────────────────────────────────

// ReturnEvent is the data transfer object for the return_events table.
// It is separate from the domain.Payment to keep concerns clean.
type ReturnEvent struct {
	ID           uuid.UUID
	PaymentID    uuid.UUID
	RCode        string
	ReceivedAt   time.Time
	RawNACHALine string
}

// AuditEntry is the data transfer object for the audit_log table.
type AuditEntry struct {
	ID        uuid.UUID
	PaymentID uuid.UUID
	FromState domain.PaymentState
	ToState   domain.PaymentState
	Reason    string
	CreatedAt time.Time
}

// ── CreatePayment ─────────────────────────────────────────────────────────────

// CreatePayment inserts a new payment record in StateInitiated.
// The caller should set p.ID before calling if they need to control the UUID;
// otherwise pass a zero UUID and the database default (gen_random_uuid()) applies.
//
// On return, p.ID is populated with the value assigned by the database.
func (r *Repository) CreatePayment(ctx context.Context, p *domain.Payment) error {
	const q = `
		INSERT INTO payments (
			id, portfolio_id, amount, state,
			return_code, representment_count, trace_number,
			created_at, updated_at, settled_at, failed_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7,
			$8, $9, $10, $11
		)
		RETURNING id, created_at, updated_at
	`

	// If the caller didn't supply an ID, generate one here so the struct is
	// populated correctly regardless of which code path generated it.
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}

	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now

	row := r.pool.QueryRow(ctx, q,
		p.ID,
		p.PortfolioID,
		p.Amount.String(), // pgx stores NUMERIC as string from shopspring/decimal
		string(p.State),
		p.ReturnCode,
		p.RepresentmentCount,
		p.TraceNumber,
		p.CreatedAt,
		p.UpdatedAt,
		p.SettledAt,
		p.FailedAt,
	)

	// RETURNING ensures the struct reflects exactly what the DB stored.
	var idStr string
	if err := row.Scan(&idStr, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return fmt.Errorf("db: CreatePayment scan: %w", err)
	}

	// Re-parse the returned ID in case the DB generated it.
	if id, err := uuid.Parse(idStr); err == nil {
		p.ID = id
	}

	return nil
}

// ── UpdatePaymentState ────────────────────────────────────────────────────────

// UpdatePaymentState transitions a payment to a new state and writes an audit
// log entry — both in a single atomic transaction.
//
// This function enforces the FSM: if the transition is not listed in
// domain.AllowedTransitions it returns an error without touching the database.
//
// toState: the target state.
// reason:  a short human-readable string logged to audit_log.reason.
//
// Side effects on p:
//   - p.State is updated to toState.
//   - p.UpdatedAt is refreshed.
//   - p.SettledAt is set if toState == StateSettled.
//   - p.FailedAt is set if toState is any terminal failure state.
func (r *Repository) UpdatePaymentState(
	ctx context.Context,
	p *domain.Payment,
	toState domain.PaymentState,
	reason string,
) error {
	if !domain.IsTransitionAllowed(p.State, toState) {
		return fmt.Errorf(
			"db: illegal state transition %s → %s for payment %s",
			p.State, toState, p.ID,
		)
	}

	now := time.Now().UTC()

	// Update nullable timestamp fields based on the target state.
	var settledAt, failedAt *time.Time
	switch toState {
	case domain.StateSettled:
		settledAt = &now
	case domain.StateFailedNonRetryable,
		domain.StateFailedRetryableExhausted,
		domain.StateComplianceEscalation:
		failedAt = &now
	}

	err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// 1. Update the payment row.
		const updatePayment = `
			UPDATE payments SET
				state        = $1,
				updated_at   = $2,
				settled_at   = COALESCE($3, settled_at),
				failed_at    = COALESCE($4, failed_at)
			WHERE id = $5
		`
		tag, err := tx.Exec(ctx, updatePayment,
			string(toState),
			now,
			settledAt,
			failedAt,
			p.ID,
		)
		if err != nil {
			return fmt.Errorf("update payment: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("payment %s not found", p.ID)
		}

		// 2. Write the audit log entry in the same transaction.
		const insertAudit = `
			INSERT INTO audit_log (payment_id, from_state, to_state, reason, created_at)
			VALUES ($1, $2, $3, $4, $5)
		`
		_, err = tx.Exec(ctx, insertAudit,
			p.ID,
			string(p.State),
			string(toState),
			reason,
			now,
		)
		if err != nil {
			return fmt.Errorf("insert audit log: %w", err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("db: UpdatePaymentState tx: %w", err)
	}

	// Reflect committed changes back onto the struct.
	p.State = toState
	p.UpdatedAt = now
	if settledAt != nil {
		p.SettledAt = settledAt
	}
	if failedAt != nil {
		p.FailedAt = failedAt
	}

	return nil
}

// ── InsertReturnEvent ─────────────────────────────────────────────────────────

// InsertReturnEvent records a NACHA return notification for a payment.
// Call this when the RDFI sends back an R-code. The raw NACHA file record line
// is stored verbatim for audit purposes.
//
// ev.ID is populated with the generated UUID on return.
func (r *Repository) InsertReturnEvent(ctx context.Context, ev *ReturnEvent) error {
	const q = `
		INSERT INTO return_events (payment_id, r_code, received_at, raw_nacha_line)
		VALUES ($1, $2, $3, $4)
		RETURNING id, received_at
	`

	now := time.Now().UTC()
	if ev.ReceivedAt.IsZero() {
		ev.ReceivedAt = now
	}

	row := r.pool.QueryRow(ctx, q,
		ev.PaymentID,
		ev.RCode,
		ev.ReceivedAt,
		ev.RawNACHALine,
	)

	var idStr string
	if err := row.Scan(&idStr, &ev.ReceivedAt); err != nil {
		return fmt.Errorf("db: InsertReturnEvent scan: %w", err)
	}
	if id, err := uuid.Parse(idStr); err == nil {
		ev.ID = id
	}

	return nil
}

// ── GetPaymentByID ────────────────────────────────────────────────────────────

// GetPaymentByID fetches a single payment by its UUID.
// Returns (nil, nil) if no row exists with that ID — callers must check for nil.
func (r *Repository) GetPaymentByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	const q = `
		SELECT
			id, portfolio_id, amount, state,
			return_code, representment_count, trace_number,
			created_at, updated_at, settled_at, failed_at
		FROM payments
		WHERE id = $1
	`

	row := r.pool.QueryRow(ctx, q, id)

	p := &domain.Payment{}
	var (
		idStr          string
		portfolioIDStr string
		amountStr      string
		stateStr       string
	)

	err := row.Scan(
		&idStr,
		&portfolioIDStr,
		&amountStr,
		&stateStr,
		&p.ReturnCode,
		&p.RepresentmentCount,
		&p.TraceNumber,
		&p.CreatedAt,
		&p.UpdatedAt,
		&p.SettledAt,
		&p.FailedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("db: GetPaymentByID scan: %w", err)
	}

	// Parse UUID strings.
	if pid, err := uuid.Parse(idStr); err == nil {
		p.ID = pid
	}
	if pid, err := uuid.Parse(portfolioIDStr); err == nil {
		p.PortfolioID = pid
	}

	// Parse the NUMERIC amount back into a decimal.
	amt, err := decimal.NewFromString(amountStr)
	if err != nil {
		return nil, fmt.Errorf("db: GetPaymentByID parse amount %q: %w", amountStr, err)
	}
	p.Amount = amt
	p.State = domain.PaymentState(stateStr)

	return p, nil
}

// ── GetAuditLogByPaymentID ────────────────────────────────────────────────────

// GetAuditLogByPaymentID returns all audit entries for a payment in
// chronological order (oldest first). Returns an empty slice if none exist.
func (r *Repository) GetAuditLogByPaymentID(ctx context.Context, paymentID uuid.UUID) ([]AuditEntry, error) {
	const q = `
		SELECT id, payment_id, from_state, to_state, reason, created_at
		FROM audit_log
		WHERE payment_id = $1
		ORDER BY created_at ASC
	`

	rows, err := r.pool.Query(ctx, q, paymentID)
	if err != nil {
		return nil, fmt.Errorf("db: GetAuditLogByPaymentID query: %w", err)
	}
	defer rows.Close()

	var entries []AuditEntry
	for rows.Next() {
		var (
			e              AuditEntry
			idStr          string
			paymentIDStr   string
			fromStateStr   string
			toStateStr     string
		)
		if err := rows.Scan(
			&idStr,
			&paymentIDStr,
			&fromStateStr,
			&toStateStr,
			&e.Reason,
			&e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: GetAuditLogByPaymentID scan: %w", err)
		}

		if id, err := uuid.Parse(idStr); err == nil {
			e.ID = id
		}
		if pid, err := uuid.Parse(paymentIDStr); err == nil {
			e.PaymentID = pid
		}
		e.FromState = domain.PaymentState(fromStateStr)
		e.ToState = domain.PaymentState(toStateStr)
		entries = append(entries, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: GetAuditLogByPaymentID rows: %w", err)
	}

	return entries, nil
}
