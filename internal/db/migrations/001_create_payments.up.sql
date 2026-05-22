-- Migration: 001_create_payments.up.sql
-- Creates the payments table, which is the central record for every ACH entry
-- tracked by the orchestrator.
--
-- Design decisions:
--   - UUID primary key: avoids leaking sequence information externally.
--   - NUMERIC(19,4) for amount: sufficient precision for USD values up to
--     $999,999,999,999,999.9999 with no floating-point rounding errors.
--   - VARCHAR for state: human-readable in raw SQL queries and pg_logs.
--   - trace_number as VARCHAR(15): ACH trace numbers are exactly 15 digits,
--     but leading zeros are significant so we must not cast to INTEGER.
--   - Nullable settled_at / failed_at: only populated when the payment reaches
--     the corresponding terminal state.

CREATE TABLE IF NOT EXISTS payments (
    id                   UUID            PRIMARY KEY DEFAULT gen_random_uuid(),
    portfolio_id         UUID            NOT NULL,
    amount               NUMERIC(19, 4)  NOT NULL CHECK (amount > 0),
    state                VARCHAR(64)     NOT NULL DEFAULT 'INITIATED',
    return_code          VARCHAR(8)      NOT NULL DEFAULT '',
    representment_count  INTEGER         NOT NULL DEFAULT 0
                            CHECK (representment_count >= 0 AND representment_count <= 2),
    trace_number         VARCHAR(15)     NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    settled_at           TIMESTAMPTZ,
    failed_at            TIMESTAMPTZ
);

-- Index for fetching all payments belonging to a portfolio (common query pattern).
CREATE INDEX IF NOT EXISTS idx_payments_portfolio_id
    ON payments (portfolio_id);

-- Index for state-based queries (e.g. "all RETURNED payments awaiting routing").
CREATE INDEX IF NOT EXISTS idx_payments_state
    ON payments (state);

-- Composite index for the retry scheduler: find retryable payments for a
-- portfolio that haven't exhausted their representment limit.
CREATE INDEX IF NOT EXISTS idx_payments_portfolio_state
    ON payments (portfolio_id, state);

COMMENT ON TABLE payments IS
    'Central ledger of ACH payment entries tracked by the retry orchestrator.';
COMMENT ON COLUMN payments.representment_count IS
    'Number of re-presentment attempts made after the initial return. Max 2 per NACHA rules.';
COMMENT ON COLUMN payments.trace_number IS
    '15-digit ACH trace number assigned by the ODFI. Stored as text to preserve leading zeros.';
