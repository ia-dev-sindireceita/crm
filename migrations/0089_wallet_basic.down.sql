-- 0089_wallet_basic.down.sql
-- Reverses 0089_wallet_basic.up.sql. Run as app_admin. Idempotent.
--
-- Order matters: drop courtesy_grant first (has no FKs), then strip the
-- wallet-aware additions from token_ledger (drop the FK-bearing wallet_id
-- column before dropping token_wallet), then drop token_wallet last.

BEGIN;

-- courtesy_grant has no dependents.
DROP TRIGGER IF EXISTS courtesy_grant_master_ops_audit ON courtesy_grant;
DROP POLICY IF EXISTS tenant_isolation_select ON courtesy_grant;
DROP TABLE IF EXISTS courtesy_grant;

-- token_ledger extensions: drop indexes, then constraints, then columns.
DROP INDEX IF EXISTS token_ledger_wallet_created_idx;
DROP INDEX IF EXISTS token_ledger_wallet_idem_idx;
ALTER TABLE token_ledger
  DROP CONSTRAINT IF EXISTS token_ledger_wallet_kind_check;
ALTER TABLE token_ledger
  DROP CONSTRAINT IF EXISTS token_ledger_wallet_idem_paired;
ALTER TABLE token_ledger DROP COLUMN IF EXISTS created_at;
ALTER TABLE token_ledger DROP COLUMN IF EXISTS external_ref;
ALTER TABLE token_ledger DROP COLUMN IF EXISTS idempotency_key;
ALTER TABLE token_ledger DROP COLUMN IF EXISTS wallet_id;

-- token_wallet last, after its FK referrer (token_ledger.wallet_id) is gone.
DROP TRIGGER IF EXISTS token_wallet_master_ops_audit ON token_wallet;
DROP POLICY IF EXISTS tenant_isolation_update ON token_wallet;
DROP POLICY IF EXISTS tenant_isolation_insert ON token_wallet;
DROP POLICY IF EXISTS tenant_isolation_select ON token_wallet;
DROP TABLE IF EXISTS token_wallet;

COMMIT;
