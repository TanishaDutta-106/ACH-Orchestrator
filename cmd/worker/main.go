// cmd/worker/main.go starts the ACH Retry Orchestrator Temporal worker.
//
// It connects to:
//   - Temporal server (TEMPORAL_HOST_PORT, default localhost:7233)
//   - PostgreSQL    (DATABASE_URL env var)
//   - Redis         (REDIS_ADDR env var, default localhost:6379)
//
// It registers PaymentWorkflow and all four activities, then blocks serving
// the "ach-payment-queue" task queue.
package main

import (
	"context"
	"log"
	"os"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/tanisha/ach-retry-orchestrator/internal/activities"
	"github.com/tanisha/ach-retry-orchestrator/internal/db"
	"github.com/tanisha/ach-retry-orchestrator/internal/domain"
	achredis "github.com/tanisha/ach-retry-orchestrator/internal/redis"
	achworkflow "github.com/tanisha/ach-retry-orchestrator/internal/workflow"
)

func main() {
	// ── Environment ──────────────────────────────────────────────────────────
	temporalHost := envOrDefault("TEMPORAL_HOST_PORT", "localhost:7233")
	databaseURL := mustEnv("DATABASE_URL")
	redisAddr := envOrDefault("REDIS_ADDR", "localhost:6379")

	ctx := context.Background()

	// ── PostgreSQL ────────────────────────────────────────────────────────────
	repo, err := db.NewRepository(ctx, databaseURL)
	if err != nil {
		log.Fatalf("worker: connect to postgres: %v", err)
	}
	defer repo.Close()
	log.Println("worker: connected to PostgreSQL")

	// ── Redis ─────────────────────────────────────────────────────────────────
	redisClient := achredis.NewClient(redisAddr)
	if err := redisClient.Ping(ctx); err != nil {
		log.Fatalf("worker: connect to redis at %s: %v", redisAddr, err)
	}
	defer redisClient.Close()
	log.Println("worker: connected to Redis")

	// ── Temporal client ───────────────────────────────────────────────────────
	tc, err := client.Dial(client.Options{
		HostPort: temporalHost,
	})
	if err != nil {
		log.Fatalf("worker: connect to temporal at %s: %v", temporalHost, err)
	}
	defer tc.Close()
	log.Printf("worker: connected to Temporal at %s", temporalHost)

	// ── Wire dependencies ─────────────────────────────────────────────────────
	acts := &activities.Activities{
		Repo:        repo,
		RedisClient: redisClient,
	}

	// ── Create and configure worker ───────────────────────────────────────────
	// domain.TemporalTaskQueue is the canonical constant ("ach-payment-queue").
	// It must NOT be hardcoded here — always import from domain.
	w := worker.New(tc, domain.TemporalTaskQueue, worker.Options{})

	// Register workflow.
	w.RegisterWorkflow(achworkflow.PaymentWorkflow)

	// Register activities via the struct so Temporal resolves the receiver.
	w.RegisterActivity(acts)

	log.Printf("worker: starting on task queue %q", domain.TemporalTaskQueue)

	// Run blocks until the process receives SIGINT/SIGTERM.
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker: run error: %v", err)
	}

	log.Println("worker: shut down cleanly")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("worker: required environment variable %q is not set", key)
	}
	return v
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
