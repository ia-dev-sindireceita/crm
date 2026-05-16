-- 0098_ai_policy_ai_summary_product_argument.down.sql
-- Reverses 0098_ai_policy_ai_summary_product_argument.up.sql. Run as
-- app_admin. Idempotent.
--
-- Drop order: product_argument first (FK→product), then ai_summary
-- (FK→conversation), product, ai_policy. tenants/users/conversation are
-- owned by their own migrations (0004/0005/0088) and stay intact.

BEGIN;

-- ---------------------------------------------------------------------------
-- product_argument: drop trigger, policies, table.
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS product_argument_master_ops_audit ON product_argument;
DROP POLICY IF EXISTS tenant_isolation_select ON product_argument;
DROP POLICY IF EXISTS tenant_isolation_insert ON product_argument;
DROP POLICY IF EXISTS tenant_isolation_update ON product_argument;
DROP POLICY IF EXISTS tenant_isolation_delete ON product_argument;
DROP TABLE IF EXISTS product_argument;

-- ---------------------------------------------------------------------------
-- ai_summary: drop trigger, policies, table.
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS ai_summary_master_ops_audit ON ai_summary;
DROP POLICY IF EXISTS tenant_isolation_select ON ai_summary;
DROP POLICY IF EXISTS tenant_isolation_insert ON ai_summary;
DROP POLICY IF EXISTS tenant_isolation_update ON ai_summary;
DROP POLICY IF EXISTS tenant_isolation_delete ON ai_summary;
DROP TABLE IF EXISTS ai_summary;

-- ---------------------------------------------------------------------------
-- product: drop trigger, policies, table. Done after product_argument
-- because of FK; explicit DROP ordering above keeps the file readable
-- regardless of CASCADE.
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS product_master_ops_audit ON product;
DROP POLICY IF EXISTS tenant_isolation_select ON product;
DROP POLICY IF EXISTS tenant_isolation_insert ON product;
DROP POLICY IF EXISTS tenant_isolation_update ON product;
DROP POLICY IF EXISTS tenant_isolation_delete ON product;
DROP TABLE IF EXISTS product;

-- ---------------------------------------------------------------------------
-- ai_policy: drop trigger, policies, table.
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS ai_policy_master_ops_audit ON ai_policy;
DROP POLICY IF EXISTS tenant_isolation_select ON ai_policy;
DROP POLICY IF EXISTS tenant_isolation_insert ON ai_policy;
DROP POLICY IF EXISTS tenant_isolation_update ON ai_policy;
DROP POLICY IF EXISTS tenant_isolation_delete ON ai_policy;
DROP TABLE IF EXISTS ai_policy;

COMMIT;
