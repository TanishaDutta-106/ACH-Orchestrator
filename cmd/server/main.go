// cmd/server/main.go — ACH Orchestrator HTTP API server.
//
// Wires together:
//   - PostgreSQL via db.NewRepository(ctx, databaseURL)
//   - Redis via achredis.NewClient(addr)
//   - Temporal client on host:port (task queue via domain.TemporalTaskQueue)
//   - chi REST API on HTTP_ADDR
//
// Environment variables (defaults shown):
//
//	DATABASE_URL   postgres://ach_user:ach_secret@localhost:5433/ach_orchestrator?sslmode=disable
//	REDIS_ADDR     localhost:6380
//	TEMPORAL_HOST_PORT  localhost:7233
//	HTTP_ADDR      :8081
//
// Graceful shutdown on SIGINT / SIGTERM with 30 s drain.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/api"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/db"
	achredis "github.com/TanishaDutta-106/ACH-Orchestrator/internal/redis"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/simulator"
)

func main() {
	// ── Config ────────────────────────────────────────────────────────────────
	databaseURL := envOrDefault("DATABASE_URL",
		"postgres://ach_user:ach_secret@localhost:5433/ach_orchestrator?sslmode=disable")
	redisAddr := envOrDefault("REDIS_ADDR", "localhost:6380")
	temporalHostPort := envOrDefault("TEMPORAL_HOST_PORT", "localhost:7233")
	httpAddr := envOrDefault("HTTP_ADDR", ":8081")

	ctx := context.Background()

	// ── PostgreSQL ────────────────────────────────────────────────────────────
	// Phase 2 constructor: db.NewRepository(ctx, databaseURL) — returns
	// (*Repository, error). It manages the pgxpool internally.
	repo, err := db.NewRepository(ctx, databaseURL)
	if err != nil {
		log.Fatalf("server: failed to connect to PostgreSQL (%s): %v", databaseURL, err)
	}
	defer repo.Close()
	log.Printf("server: PostgreSQL connected")

	// ── Redis ─────────────────────────────────────────────────────────────────
	// Phase 2 constructor: achredis.NewClient(addr) — returns *achredis.Client.
	rc := achredis.NewClient(redisAddr)
	if err := rc.Ping(ctx); err != nil {
		log.Fatalf("server: failed to connect to Redis (%s): %v", redisAddr, err)
	}
	defer rc.Close()
	log.Printf("server: Redis connected at %s", redisAddr)

	// ── Temporal ──────────────────────────────────────────────────────────────
	// Task queue name comes from domain.TemporalTaskQueue — the same constant
	// the worker uses. Never a raw string literal.
	temporalClient, err := client.Dial(client.Options{
		HostPort: temporalHostPort,
	})
	if err != nil {
		log.Fatalf("server: failed to connect to Temporal (%s): %v", temporalHostPort, err)
	}
	defer temporalClient.Close()
	log.Printf("server: Temporal connected at %s", temporalHostPort)

	// ── Wiring ────────────────────────────────────────────────────────────────
	sim := simulator.New(temporalClient)
	handler := api.New(temporalClient, repo, sim)

	srv := &http.Server{
		Addr:         httpAddr,
		Handler:      handler.Router(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ── Start ─────────────────────────────────────────────────────────────────
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("server: HTTP listening on %s", httpAddr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// ── Shutdown ──────────────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		log.Printf("server: signal %s received — shutting down", sig)
	case err := <-serverErr:
		log.Fatalf("server: fatal: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server: forced shutdown: %v", err)
	}
	log.Printf("server: clean shutdown complete")
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
