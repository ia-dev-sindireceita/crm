-- 0015_drop_audit_log.up.sql
-- SIN-62424: drop the legacy `audit_log` table after the split-audit
-- adapter (SIN-62252) was retired. The replacement tables
-- `audit_log_security` and `audit_log_data` live in 0012; the LGPD
-- purge job lives in `internal/audit/purge`.
--
-- DROP TABLE cascades:
--   * the master_ops_audit trigger attached to it,
--   * every grant the table carried (including the legacy
--     INSERT-on-audit_log grant 0009 issued to `app_audit`).
-- The `app_audit` role itself remains: 0014 already granted INSERT on
-- audit_log_security / audit_log_data, so the role keeps its
-- least-privileged surface against the new ledgers.
--
-- Run as app_admin. Idempotent (DROP IF EXISTS).

BEGIN;

DROP TABLE IF EXISTS audit_log;

COMMIT;
