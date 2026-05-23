package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/redis"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/db"
	"go.temporal.io/sdk/client"
)

// healthResponse mirrors the required JSON shape exactly.
type healthResponse struct {
	Status   string `json:"status"`
	Postgres string `json:"postgres"`
	Redis    string `json:"redis"`
	Temporal string `json:"temporal"`
}

// HealthHandler returns a handler that checks all three dependencies concurrently.
// Returns 200 when all pass, 503 when any critical dependency is down.
func HealthHandler(repo *db.Repository, rc *redis.Client, tc client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		var (
			mu       sync.Mutex
			pgStatus = "ok"
			rdStatus = "ok"
			tpStatus = "ok"
			wg       sync.WaitGroup
		)

		// ── Postgres ──────────────────────────────────────────────────────────
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := repo.Ping(ctx); err != nil {
				mu.Lock()
				pgStatus = "error: " + err.Error()
				mu.Unlock()
			}
		}()

		// ── Redis ─────────────────────────────────────────────────────────────
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rc.Ping(ctx); err != nil {
				mu.Lock()
				rdStatus = "error: " + err.Error()
				mu.Unlock()
			}
		}()

		// ── Temporal ──────────────────────────────────────────────────────────
		wg.Add(1)
		go func() {
			defer wg.Done()
			// CheckHealth is a lightweight gRPC call; no workflow is started.
			_, err := tc.CheckHealth(ctx, nil)
			if err != nil {
				mu.Lock()
				tpStatus = "error: " + err.Error()
				mu.Unlock()
			}
		}()

		wg.Wait()

		overall := "ok"
		statusCode := http.StatusOK
		if pgStatus != "ok" || rdStatus != "ok" || tpStatus != "ok" {
			overall = "degraded"
			statusCode = http.StatusServiceUnavailable
		}

		resp := healthResponse{
			Status:   overall,
			Postgres: pgStatus,
			Redis:    rdStatus,
			Temporal: tpStatus,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
