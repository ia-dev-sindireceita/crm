-- 0099_ai_policy_audit.down.sql
-- Reverses 0099_ai_policy_audit.up.sql. Drops the audit table and its
-- two indexes. The down migration is irreversible — once the table is
-- gone the per-field history is gone with it — so production rollback
-- should restore from backup rather than rely on this script.

BEGIN;

DROP POLICY IF EXISTS master_ops_full ON ai_policy_audit;
DROP POLICY IF EXISTS tenant_isolation_insert ON ai_policy_audit;
DROP POLICY IF EXISTS tenant_isolation_select ON ai_policy_audit;
DROP INDEX IF EXISTS ai_policy_audit_tenant_scope_created_idx;
DROP INDEX IF EXISTS ai_policy_audit_tenant_created_idx;
DROP TABLE IF EXISTS ai_policy_audit;

COMMIT;
