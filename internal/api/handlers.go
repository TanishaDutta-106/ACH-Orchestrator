// Package api implements the chi-based REST API for the ACH Payment
// Retry Orchestrator.
//
// Endpoints:
//
//	POST   /payments              — validate input, start PaymentWorkflow
//	GET    /payments/{id}         — current state, representment count, trace number
//	POST   /payments/{id}/return  — signal ReturnSignal to running workflow
//	GET    /payments/{id}/audit   — full audit log from PostgreSQL
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.temporal.io/sdk/client"

	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/db"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/domain"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/simulator"
	achworkflow "github.com/TanishaDutta-106/ACH-Orchestrator/internal/workflow"
)

// ── Validation regexes ────────────────────────────────────────────────────────

var (
	reAccountNumber = regexp.MustCompile(`^\d{4,17}$`)
	reRoutingNumber = regexp.MustCompile(`^\d{9}$`)
	reRCode         = regexp.MustCompile(`^R\d{2}$`)
)

// ── Request / Response types ──────────────────────────────────────────────────

// CreatePaymentRequest is the body for POST /payments.
type CreatePaymentRequest struct {
	Amount        string `json:"amount"`         // positive decimal, e.g. "100.50"
	AccountNumber string `json:"account_number"` // 4–17 digits (validated, not stored on Payment)
	RoutingNumber string `json:"routing_number"` // exactly 9 digits (validated, not stored on Payment)
	Description   string `json:"description,omitempty"`
}

// CreatePaymentResponse is returned on 201.
type CreatePaymentResponse struct {
	PaymentID string `json:"payment_id"`
	State     string `json:"state"`
}

// PaymentStatusResponse is returned by GET /payments/{id}.
type PaymentStatusResponse struct {
	PaymentID          string     `json:"payment_id"`
	State              string     `json:"state"`
	RepresentmentCount int        `json:"representment_count"`
	TraceNumber        string     `json:"trace_number,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	SettledAt          *time.Time `json:"settled_at,omitempty"`
	FailedAt           *time.Time `json:"failed_at,omitempty"`
}

// ReturnRequest is the body for POST /payments/{id}/return.
type ReturnRequest struct {
	RCode         string `json:"r_code"`         // e.g. "R01"
	RoutingNumber string `json:"routing_number"` // 9 digits
	AccountNumber string `json:"account_number"` // 4–17 digits
	AmountCents   int64  `json:"amount_cents"`   // original amount in cents
}

// AuditEntryResponse is one row from the audit_log table.
type AuditEntryResponse struct {
	ID        string    `json:"id"`
	PaymentID string    `json:"payment_id"`
	FromState string    `json:"from_state"`
	ToState   string    `json:"to_state"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrorResponse is the standard error envelope.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ── Handler ───────────────────────────────────────────────────────────────────

// Handler holds all runtime dependencies for the HTTP handlers.
type Handler struct {
	temporalClient client.Client
	repo           *db.Repository
	sim            *simulator.ReturnSimulator
}

// New constructs a Handler. All three arguments are required.
func New(
	temporalClient client.Client,
	repo *db.Repository,
	sim *simulator.ReturnSimulator,
) *Handler {
	return &Handler{
		temporalClient: temporalClient,
		repo:           repo,
		sim:            sim,
	}
}

// Router returns a fully-configured chi.Router.
func (h *Handler) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Route("/payments", func(r chi.Router) {
		r.Post("/", h.createPayment)

		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.getPayment)
			r.Post("/return", h.postReturn)
			r.Get("/audit", h.getAudit)
		})
	})

	return r
}

// ── POST /payments ────────────────────────────────────────────────────────────

func (h *Handler) createPayment(w http.ResponseWriter, r *http.Request) {
	var req CreatePaymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// ── Validate all fields ───────────────────────────────────────────────────
	if req.Amount == "" {
		writeError(w, http.StatusBadRequest, "amount is required")
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || !amount.IsPositive() {
		writeError(w, http.StatusBadRequest, "amount must be a positive decimal number")
		return
	}
	if req.AccountNumber == "" {
		writeError(w, http.StatusBadRequest, "account_number is required")
		return
	}
	if !reAccountNumber.MatchString(req.AccountNumber) {
		writeError(w, http.StatusBadRequest, "account_number must be 4–17 digits")
		return
	}
	if req.RoutingNumber == "" {
		writeError(w, http.StatusBadRequest, "routing_number is required")
		return
	}
	if !reRoutingNumber.MatchString(req.RoutingNumber) {
		writeError(w, http.StatusBadRequest, "routing_number must be exactly 9 digits")
		return
	}

	// ── Build domain payment ──────────────────────────────────────────────────
	// domain.Payment does not have AccountNumber/RoutingNumber/Description
	// fields — those are validated here but passed to the workflow input only.
	paymentID := uuid.New()
	now := time.Now().UTC()
	payment := &domain.Payment{
		ID:        paymentID,
		Amount:    amount,
		State:     domain.StateInitiated,
		CreatedAt: now,
		UpdatedAt: now,
	}

	// ── Persist ───────────────────────────────────────────────────────────────
	if err := h.repo.CreatePayment(r.Context(), payment); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist payment: "+err.Error())
		return
	}

	// ── Start workflow ────────────────────────────────────────────────────────
	// WorkflowID = paymentID.String() so the return endpoint can signal by ID.
	// Task queue via domain.TemporalTaskQueue — never a raw string literal.
	options := client.StartWorkflowOptions{
		ID:        paymentID.String(),
		TaskQueue: domain.TemporalTaskQueue,
	}
	_, err = h.temporalClient.ExecuteWorkflow(
		r.Context(),
		options,
		achworkflow.PaymentWorkflow,
		achworkflow.PaymentWorkflowInput{
			PaymentID:     paymentID,
			Amount:        req.Amount,
			AccountNumber: req.AccountNumber,
			RoutingNumber: req.RoutingNumber,
		},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start workflow: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, CreatePaymentResponse{
		PaymentID: paymentID.String(),
		State:     string(domain.StateInitiated),
	})
}

// ── GET /payments/{id} ────────────────────────────────────────────────────────

func (h *Handler) getPayment(w http.ResponseWriter, r *http.Request) {
	paymentID, ok := parseUUID(w, r)
	if !ok {
		return
	}

	payment, err := h.repo.GetPaymentByID(r.Context(), paymentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error: "+err.Error())
		return
	}
	if payment == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("payment %s not found", paymentID))
		return
	}

	writeJSON(w, http.StatusOK, PaymentStatusResponse{
		PaymentID:          payment.ID.String(),
		State:              string(payment.State),
		RepresentmentCount: payment.RepresentmentCount,
		TraceNumber:        payment.TraceNumber,
		CreatedAt:          payment.CreatedAt,
		UpdatedAt:          payment.UpdatedAt,
		SettledAt:          payment.SettledAt,
		FailedAt:           payment.FailedAt,
	})
}

// ── POST /payments/{id}/return ────────────────────────────────────────────────

func (h *Handler) postReturn(w http.ResponseWriter, r *http.Request) {
	paymentID, ok := parseUUID(w, r)
	if !ok {
		return
	}

	var req ReturnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if !reRCode.MatchString(req.RCode) {
		writeError(w, http.StatusBadRequest, "r_code must match R## format, e.g. R01")
		return
	}
	if !reRoutingNumber.MatchString(req.RoutingNumber) {
		writeError(w, http.StatusBadRequest, "routing_number must be exactly 9 digits")
		return
	}
	if !reAccountNumber.MatchString(req.AccountNumber) {
		writeError(w, http.StatusBadRequest, "account_number must be 4–17 digits")
		return
	}
	if req.AmountCents <= 0 {
		writeError(w, http.StatusBadRequest, "amount_cents must be positive")
		return
	}

	if err := h.sim.SimulateReturn(
		r.Context(),
		paymentID,
		req.RoutingNumber,
		req.AccountNumber,
		req.AmountCents,
		req.RCode,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to simulate return: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":     "return_signaled",
		"payment_id": paymentID.String(),
		"r_code":     req.RCode,
	})
}

// ── GET /payments/{id}/audit ─────────────────────────────────────────────────

func (h *Handler) getAudit(w http.ResponseWriter, r *http.Request) {
	paymentID, ok := parseUUID(w, r)
	if !ok {
		return
	}

	// Existence check — return 404 rather than an empty array for unknown IDs.
	payment, err := h.repo.GetPaymentByID(r.Context(), paymentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error: "+err.Error())
		return
	}
	if payment == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("payment %s not found", paymentID))
		return
	}

	logs, err := h.repo.GetAuditLogByPaymentID(r.Context(), paymentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to fetch audit log: "+err.Error())
		return
	}

	resp := make([]AuditEntryResponse, 0, len(logs))
	for _, l := range logs {
		resp = append(resp, AuditEntryResponse{
			ID:        l.ID.String(),
			PaymentID: l.PaymentID.String(),
			FromState: string(l.FromState),
			ToState:   string(l.ToState),
			Reason:    l.Reason,
			CreatedAt: l.CreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// parseUUID extracts and parses the {id} URL parameter.
// Writes 400 and returns false on failure.
func parseUUID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := chi.URLParam(r, "id")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "payment id is required")
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid payment id %q: must be a UUID", raw))
		return uuid.UUID{}, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}