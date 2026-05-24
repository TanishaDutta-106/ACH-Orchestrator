package db

import (
	"context"

	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/domain"
)

// GetAllPayments returns up to limit payments ordered by created_at DESC.
// It is intentionally placed in a separate file so that repository.go is not modified.
func (r *Repository) GetAllPayments(ctx context.Context, limit int) ([]domain.Payment, error) {
	const q = `
		SELECT
			id, portfolio_id, amount, state, return_code,
			representment_count, trace_number,
			created_at, updated_at, settled_at, failed_at
		FROM payments
		ORDER BY created_at DESC
		LIMIT $1`

	rows, err := r.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var payments []domain.Payment
	for rows.Next() {
		var p domain.Payment
		if err := rows.Scan(
			&p.ID,
			&p.PortfolioID,
			&p.Amount,
			&p.State,
			&p.ReturnCode,
			&p.RepresentmentCount,
			&p.TraceNumber,
			&p.CreatedAt,
			&p.UpdatedAt,
			&p.SettledAt,
			&p.FailedAt,
		); err != nil {
			return nil, err
		}
		payments = append(payments, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return payments, nil
}
