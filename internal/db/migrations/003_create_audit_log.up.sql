-- Migration: 003_create_audit_log.up.sql
-- Immutable audit trail for every payment state transition.
--
-- Every call to UpdatePaymentState in the repository layer writes one row here
-- within the same transaction. This gives us a complete, tamper-evident history
-- of how each payment moved through the FSM.
--
-- This table is append-only. Never UPDATE or DELETE rows. If you need to
-- correct an audit entry, write a new row with reason = 'correction: <detail>'.

CREATE TABLE IF NOT EXISTS audit_log (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id   UUID         NOT NULL REFERENCES payments(id) ON DELETE CASCADE,
    from_state   VARCHAR(64)  NOT NULL,
    to_state     VARCHAR(64)  NOT NULL,
    -- reason should be a short human-readable string explaining why the
    -- transition occurred, e.g. "R01 return received", "representment attempt 1".
    reason       TEXT         NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Primary access pattern: fetch the full state history for a single payment.
CREATE INDEX IF NOT EXISTS idx_audit_log_payment_id
    ON audit_log (payment_id, created_at ASC);

-- Allows querying transitions into a specific state across all payments —
-- useful for dashboards ("how many payments reached COMPLIANCE_ESCALATION today?").
CREATE INDEX IF NOT EXISTS idx_audit_log_to_state
    ON audit_log (to_state, created_at DESC);

COMMENT ON TABLE audit_log IS
    'Immutable ordered log of every payment state transition. Never update or delete rows.';
COMMENT ON COLUMN audit_log.reason IS
    'Short human-readable explanation of why this transition was triggered.';
