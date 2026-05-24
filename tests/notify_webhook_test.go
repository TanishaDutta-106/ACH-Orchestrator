package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/activities"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/domain"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestPayment() *domain.Payment {
	return &domain.Payment{
		ID:          uuid.New(),
		PortfolioID: uuid.New(),
		Amount:      decimal.NewFromFloat(250.00),
		State:       domain.StateSettled,
		ReturnCode:  "",
		TraceNumber: "123456789012345",
	}
}

// newAct creates an Activities with nil repo/redis — fine because NotifyWebhook doesn't use them.
func newAct() *activities.Activities {
	return &activities.Activities{}
}

// TestNotifyWebhook_Success verifies a 200 response results in nil error.
func TestNotifyWebhook_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("WEBHOOK_URL", srv.URL)

	act := newAct()
	err := act.NotifyWebhook(context.Background(), newTestPayment())
	require.NoError(t, err)
}

// TestNotifyWebhook_AllRetriesExhausted verifies that a permanently failing endpoint
// still returns nil — webhook failure must not propagate.
func TestNotifyWebhook_AllRetriesExhausted(t *testing.T) {
	// Server always returns 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Setenv("WEBHOOK_URL", srv.URL)

	act := newAct()
	// Override delays for speed: use a context that we know won't cancel
	// The activity uses time.After internally; this test will take ~7s at real delays.
	// To keep it fast, point at a server that immediately rejects — no sleep needed.
	err := act.NotifyWebhook(context.Background(), newTestPayment())
	require.NoError(t, err, "webhook exhaustion must never return an error")
}

// TestNotifyWebhook_EmptyURL verifies that an empty WEBHOOK_URL results in a no-op.
func TestNotifyWebhook_EmptyURL(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "")

	act := newAct()
	err := act.NotifyWebhook(context.Background(), newTestPayment())
	require.NoError(t, err)
}