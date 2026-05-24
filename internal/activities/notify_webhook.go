package activities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/domain"
	"github.com/shopspring/decimal"
)

// WebhookPayload is the JSON body POSTed to WEBHOOK_URL on terminal state transitions.
type WebhookPayload struct {
	PaymentID   string          `json:"payment_id"`
	State       string          `json:"state"`
	ReturnCode  string          `json:"return_code"`
	Amount      decimal.Decimal `json:"amount"`
	TraceNumber string          `json:"trace_number"`
	Timestamp   time.Time       `json:"timestamp"`
}

// NotifyWebhook POSTs a terminal-state notification to the configured WEBHOOK_URL.
// If WEBHOOK_URL is empty, it returns nil immediately (webhook disabled).
// All retry failures are logged and swallowed — this activity NEVER returns a non-nil error.
func (a *Activities) NotifyWebhook(ctx context.Context, p *domain.Payment) error {
	url := os.Getenv("WEBHOOK_URL")
	if url == "" {
		return nil // webhook disabled — skip silently
	}

	payload := WebhookPayload{
		PaymentID:   p.ID.String(),
		State:       string(p.State),
		ReturnCode:  p.ReturnCode,
		Amount:      p.Amount,
		TraceNumber: p.TraceNumber,
		Timestamp:   time.Now().UTC(),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		// Should never happen with this struct, but swallow it
		log.Printf("[NotifyWebhook] marshal error for payment %s: %v", p.ID, err)
		return nil
	}

	delays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	var lastErr error

	for attempt, delay := range delays {
		lastErr = doPost(ctx, url, body)
		if lastErr == nil {
			return nil
		}
		log.Printf("[NotifyWebhook] attempt %d failed for payment %s: %v", attempt+1, p.ID, lastErr)
		if attempt < len(delays)-1 {
			select {
			case <-ctx.Done():
				log.Printf("[NotifyWebhook] context cancelled for payment %s", p.ID)
				return nil // swallow — do not block workflow
			case <-time.After(delay):
			}
		}
	}

	// All 3 attempts exhausted — log and swallow
	log.Printf("[NotifyWebhook] all retries exhausted for payment %s: %v", p.ID, lastErr)
	return nil
}

func doPost(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("non-2xx status: %d", resp.StatusCode)
	}
	return nil
}