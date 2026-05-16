-- 0097_subscription_plan_invoice_master_grant.down.sql
-- Reverses 0097_subscription_plan_invoice_master_grant.up.sql. Run as
-- app_admin. Idempotent.
--
-- Order matters: drop the token_ledger extensions FIRST (the new FK on
-- master_grant_id needs to disappear before master_grant can be dropped),
-- then drop invoice (FK→subscription), subscription (FK→plan), plan,
-- master_grant_request (no FK dependents), and master_grant last.
-- tenants/users are owned by 0004/0005 and stay intact.

BEGIN;

-- ---------------------------------------------------------------------------
-- token_ledger extensions: drop indexes, then constraints, then columns.
-- ---------------------------------------------------------------------------

DROP INDEX IF EXISTS token_ledger_master_grant_id_idx;

ALTER TABLE token_ledger
  DROP CONSTRAINT IF EXISTS token_ledger_master_grant_pairing;
ALTER TABLE token_ledger
  DROP CONSTRAINT IF EXISTS token_ledger_source_check;

ALTER TABLE token_ledger DROP COLUMN IF EXISTS master_grant_id;
ALTER TABLE token_ledger DROP COLUMN IF EXISTS source;

-- ---------------------------------------------------------------------------
-- invoice: drop trigger, policy, table.
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS invoice_master_ops_audit ON invoice;
DROP POLICY IF EXISTS tenant_isolation_select ON invoice;
DROP TABLE IF EXISTS invoice;

-- ---------------------------------------------------------------------------
-- subscription: drop trigger, policy, table.
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS subscription_master_ops_audit ON subscription;
DROP POLICY IF EXISTS tenant_isolation_select ON subscription;
DROP TABLE IF EXISTS subscription;

-- ---------------------------------------------------------------------------
-- plan: no RLS / no trigger, just drop.
-- ---------------------------------------------------------------------------

DROP TABLE IF EXISTS plan;

-- ---------------------------------------------------------------------------
-- master_grant_request: drop trigger then table. Done before
-- master_grant only for symmetry; there is no FK between the two.
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS master_grant_request_master_ops_audit ON master_grant_request;
DROP TABLE IF EXISTS master_grant_request;

-- ---------------------------------------------------------------------------
-- master_grant: drop trigger, policy, table LAST so token_ledger.FK is
-- already gone above.
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS master_grant_master_ops_audit ON master_grant;
DROP POLICY IF EXISTS tenant_isolation_select ON master_grant;
DROP TABLE IF EXISTS master_grant;

COMMIT;
