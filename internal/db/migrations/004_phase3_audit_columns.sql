-- Migration 004: Ensure payment_audit_log has all columns needed by Phase 3.
-- This migration is idempotent — all changes use IF NOT EXISTS / DO blocks.
--
-- Phase 1 created payment_audit_log with: id, payment_id, from_state,
-- to_state, occurred_at. Phase 3 adds r_code and reason columns if they
-- are not already present (Phase 2 may have added them).

DO $$
BEGIN
    -- Add r_code column if missing.
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'payment_audit_log'
          AND column_name = 'r_code'
    ) THEN
        ALTER TABLE payment_audit_log
            ADD COLUMN r_code TEXT NOT NULL DEFAULT '';
    END IF;

    -- Add reason column if missing.
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'payment_audit_log'
          AND column_name = 'reason'
    ) THEN
        ALTER TABLE payment_audit_log
            ADD COLUMN reason TEXT NOT NULL DEFAULT '';
    END IF;

    -- Add next_retry_at column to payments if missing (needed by Phase 3 GET endpoint).
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'payments'
          AND column_name = 'next_retry_at'
    ) THEN
        ALTER TABLE payments
            ADD COLUMN next_retry_at TIMESTAMPTZ;
    END IF;

    -- Add trace_number column to payments if missing.
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'payments'
          AND column_name = 'trace_number'
    ) THEN
        ALTER TABLE payments
            ADD COLUMN trace_number TEXT NOT NULL DEFAULT '';
    END IF;
END;
$$;

-- Index on payment_audit_log.payment_id for fast audit log retrieval.
CREATE INDEX IF NOT EXISTS idx_audit_log_payment_id
    ON payment_audit_log(payment_id, occurred_at ASC);

-- Index on payments.state for workflow state queries.
CREATE INDEX IF NOT EXISTS idx_payments_state
    ON payments(state);
