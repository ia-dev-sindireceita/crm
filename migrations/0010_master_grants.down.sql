-- 0010_master_grants.down.sql
-- Rolls back 0010_master_grants.up.sql. Drops the audit log first so the
-- grant_id reference becomes orphan-free, then the grants table.
BEGIN;
DROP INDEX IF EXISTS idx_master_grants_audit_principal_at;
DROP INDEX IF EXISTS idx_master_grants_audit_grant_id;
DROP TABLE IF EXISTS master_grants_audit_log;
DROP INDEX IF EXISTS idx_master_grants_master_window;
DROP INDEX IF EXISTS idx_master_grants_subscription_window;
DROP TABLE IF EXISTS master_grants;
COMMIT;
