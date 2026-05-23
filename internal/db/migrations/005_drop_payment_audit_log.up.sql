-- Migration: 005_drop_payment_audit_log.up.sql
-- Removes the legacy payment_audit_log table created in Phase 1.
-- The canonical audit table is audit_log (created in 003).
-- All Phase 4 code reads and writes audit_log exclusively.

DROP TABLE IF EXISTS payment_audit_log;