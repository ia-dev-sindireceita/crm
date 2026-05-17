-- 0101_ai_policy_consent.down.sql
-- Reverses 0101_ai_policy_consent.up.sql. Run as app_admin. Idempotent.
--
-- Safe to drop unconditionally: no other table in the schema has an FK
-- pointing at ai_policy_consent yet (the IA gate reads-only via the
-- service layer).

BEGIN;

DROP TRIGGER IF EXISTS ai_policy_consent_master_ops_audit ON ai_policy_consent;
DROP POLICY IF EXISTS tenant_isolation_select ON ai_policy_consent;
DROP POLICY IF EXISTS tenant_isolation_insert ON ai_policy_consent;
DROP POLICY IF EXISTS tenant_isolation_update ON ai_policy_consent;
DROP POLICY IF EXISTS tenant_isolation_delete ON ai_policy_consent;
DROP TABLE IF EXISTS ai_policy_consent;

COMMIT;
