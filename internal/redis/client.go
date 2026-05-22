// Package redis wraps go-redis/v9 and provides the idempotency store used by
// Phase 2 ACH activities.  All methods are safe for concurrent use.
package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const (
	// TraceKeyPrefix is prepended to every trace-number key stored in Redis.
	TraceKeyPrefix = "ach:trace:"

	// TraceTTL is the idempotency window for ACH trace numbers.
	// NACHA requires a minimum of 5 banking days; 7 calendar days is a safe,
	// common production choice.
	TraceTTL = 7 * 24 * time.Hour
)

// ErrTraceExists is returned by CheckIdempotency when the trace number is
// already present in Redis — the caller should short-circuit and NOT re-submit.
var ErrTraceExists = errors.New("trace number already submitted")

// Client wraps a go-redis client and exposes only the operations Phase 2 needs.
type Client struct {
	rdb *goredis.Client
}

// NewClient constructs a Redis Client from the given addr (e.g. "localhost:6379").
func NewClient(addr string) *Client {
	rdb := goredis.NewClient(&goredis.Options{
		Addr: addr,
	})
	return &Client{rdb: rdb}
}

// Ping verifies connectivity.  Call this at worker startup.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close shuts down the underlying connection pool.
func (c *Client) Close() error {
	return c.rdb.Close()
}

// traceKey returns the Redis key for the given trace number.
func traceKey(traceNumber string) string {
	return fmt.Sprintf("%s%s", TraceKeyPrefix, traceNumber)
}

// CheckIdempotency returns ErrTraceExists if traceNumber is already recorded in
// Redis, nil if it has never been seen.  Any other error is a transient Redis
// failure that the caller (activity) should surface to Temporal for retry.
func (c *Client) CheckIdempotency(ctx context.Context, traceNumber string) error {
	val, err := c.rdb.Exists(ctx, traceKey(traceNumber)).Result()
	if err != nil {
		return fmt.Errorf("redis EXISTS %s: %w", traceNumber, err)
	}
	if val > 0 {
		return ErrTraceExists
	}
	return nil
}

// StoreTraceNumber writes traceNumber into Redis with TraceTTL (7 days).
// Uses SET NX so that a duplicate call from an activity retry is a no-op.
func (c *Client) StoreTraceNumber(ctx context.Context, traceNumber string) error {
	ok, err := c.rdb.SetNX(ctx, traceKey(traceNumber), "1", TraceTTL).Result()
	if err != nil {
		return fmt.Errorf("redis SETNX %s: %w", traceNumber, err)
	}
	if !ok {
		// Key already existed — idempotent, not an error.
		return nil
	}
	return nil
}
