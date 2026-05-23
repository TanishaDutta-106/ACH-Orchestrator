package db

import "context"

// Ping verifies the PostgreSQL connection is alive.
// Called exclusively by the health check handler.
func (r *Repository) Ping(ctx context.Context) error {
	return r.pool.Ping(ctx)
}
