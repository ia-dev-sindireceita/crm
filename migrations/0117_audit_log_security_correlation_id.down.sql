-- 0117_audit_log_security_correlation_id.down.sql
-- Inverse of 0117. Drops the index first (so the column drop does not
-- need to walk the partial index), then the column. Idempotent.

BEGIN;

DROP INDEX IF EXISTS audit_log_security_correlation_id_idx;

ALTER TABLE audit_log_security
  DROP COLUMN IF EXISTS correlation_id;

COMMIT;
