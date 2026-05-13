-- 0083_split_audit_log.down.sql (re-landed from legacy 0012 per ADR 0086)
-- Reverse of 0083_split_audit_log.up.sql.
--
-- Notes on rollback semantics:
--   * Rollback drops the unified view first (depends on both tables).
--   * Drops audit_log_security and audit_log_data — any rows already
--     written to them are LOST. The legacy audit_log table is left
--     in place because 0012.up did not touch it; if 0012 was rolled
--     back the legacy writer keeps working.
--   * IF EXISTS makes this safe to run twice in a row.
--
-- Run as app_admin. Idempotent.

BEGIN;

DROP VIEW IF EXISTS audit_log_unified;

DROP TRIGGER IF EXISTS audit_log_data_master_ops_audit ON audit_log_data;
DROP TABLE IF EXISTS audit_log_data;

DROP TRIGGER IF EXISTS audit_log_security_master_ops_audit ON audit_log_security;
DROP TABLE IF EXISTS audit_log_security;

COMMIT;
