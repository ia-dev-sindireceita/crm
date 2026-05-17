-- 0103_funnel_rule_applications.down.sql
--
-- Reverse of the up migration. Drops policies/trigger/indexes implicitly
-- via DROP TABLE; spelled out here defensively in case operators apply
-- the down with the table missing (CASCADE handles the implicit teardown
-- but the explicit DROPs are idempotent).

DROP TRIGGER IF EXISTS funnel_rule_applications_master_ops_audit ON funnel_rule_applications;
DROP POLICY IF EXISTS tenant_isolation_delete ON funnel_rule_applications;
DROP POLICY IF EXISTS tenant_isolation_update ON funnel_rule_applications;
DROP POLICY IF EXISTS tenant_isolation_insert ON funnel_rule_applications;
DROP POLICY IF EXISTS tenant_isolation_select ON funnel_rule_applications;
DROP INDEX IF EXISTS funnel_rule_applications_message_idx;
DROP INDEX IF EXISTS funnel_rule_applications_tenant_applied_idx;
DROP TABLE IF EXISTS funnel_rule_applications;
