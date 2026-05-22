-- Migration: 002_create_return_events.up.sql
-- Stores every ACH return notification received from the RDFI.
--
-- A single payment may have multiple return events if it is re-presented and
-- returned again. This table is append-only — rows are never updated or deleted.
--
-- raw_nacha_line stores the verbatim 94-character NACHA file record for the
-- return entry. This is required for regulatory audit trails and dispute
-- resolution. Do not truncate or sanitize it.

CREATE TABLE IF NOT EXISTS return_events (
    id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id     UUID         NOT NULL REFERENCES payments(id) ON DELETE CASCADE,
    r_code         VARCHAR(8)   NOT NULL,
    received_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- The raw 94-character fixed-width NACHA record line. VARCHAR(94) enforces
    -- the NACHA file format constraint at the database level.
    raw_nacha_line VARCHAR(94)  NOT NULL DEFAULT ''
);

-- Most queries against return_events filter by payment_id.
CREATE INDEX IF NOT EXISTS idx_return_events_payment_id
    ON return_events (payment_id);

-- Allows querying all returns for a given R-code (useful for compliance
-- reporting: "how many R16 returns did we receive this month?").
CREATE INDEX IF NOT EXISTS idx_return_events_r_code
    ON return_events (r_code);

COMMENT ON TABLE return_events IS
    'Append-only log of every ACH return entry (R-code) received for a payment.';
COMMENT ON COLUMN return_events.raw_nacha_line IS
    'Verbatim 94-character NACHA file return record. Required for audit and dispute resolution.';
