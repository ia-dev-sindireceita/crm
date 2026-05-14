-- 0089_wallet_basic.up.sql
-- Fase 1 / SIN-62725: foundational wallet tables for the MVP token economy.
--
-- Three concerns ship together in this migration so the relationship between
-- them is enforced by the database from day one:
--
--   * token_wallet     — one row per tenant, holds balance + reserved (F30
--                        atomic-reserve model from SIN-62227 / SIN-62239).
--   * token_ledger     — the canonical append-only journal that already
--                        existed as the RLS demo (0003); we extend it with
--                        wallet-aware columns (wallet_id, idempotency_key,
--                        external_ref, created_at). The columns are NULLABLE
--                        with a paired CHECK so the SIN-62232 RLS demo
--                        fixtures (which insert with only tenant_id+kind+
--                        amount) keep working, while wallet-aware callers
--                        MUST supply both wallet_id and idempotency_key. The
--                        partial UNIQUE index gives the AC's "1 success +
--                        99 conflicts" guarantee for concurrent inserts
--                        sharing an idempotency_key.
--   * courtesy_grant   — the initial wallet credit issued to a tenant when
--                        it is created (PR11 / SIN-62730 wires this into
--                        tenant onboarding).
--
-- Run as app_admin. Idempotent (CREATE TABLE IF NOT EXISTS, ADD COLUMN IF
-- NOT EXISTS, DROP CONSTRAINT IF EXISTS, etc).

BEGIN;

-- ---------------------------------------------------------------------------
-- token_wallet: per-tenant wallet (one row per tenant_id).
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS token_wallet (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL UNIQUE,
  balance     bigint NOT NULL DEFAULT 0 CHECK (balance >= 0),
  reserved    bigint NOT NULL DEFAULT 0 CHECK (reserved >= 0),
  version     bigint NOT NULL DEFAULT 0,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE token_wallet OWNER TO app_admin;

ALTER TABLE token_wallet ENABLE ROW LEVEL SECURITY;
ALTER TABLE token_wallet FORCE ROW LEVEL SECURITY;

REVOKE ALL ON token_wallet FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON token_wallet TO app_runtime;
-- Runtime never deletes wallet rows; they live for the lifetime of the
-- tenant. The tenant-delete cascade is handled by an explicit master_ops
-- DELETE so the audit trail captures it.
REVOKE DELETE ON token_wallet FROM app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON token_wallet TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON token_wallet;
CREATE POLICY tenant_isolation_select ON token_wallet
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON token_wallet;
CREATE POLICY tenant_isolation_insert ON token_wallet
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON token_wallet;
CREATE POLICY tenant_isolation_update ON token_wallet
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS token_wallet_master_ops_audit ON token_wallet;
CREATE TRIGGER token_wallet_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON token_wallet
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- token_ledger: extend the SIN-62232 RLS-demo table with wallet semantics.
-- ---------------------------------------------------------------------------

-- Wallet linkage. NULLABLE so legacy fixtures from withtenant_test.go
-- (which insert with only tenant_id/kind/amount) continue to pass; the
-- paired CHECK below forbids partial wallet-aware writes.
ALTER TABLE token_ledger
  ADD COLUMN IF NOT EXISTS wallet_id uuid REFERENCES token_wallet(id);

-- Idempotency key for the wallet-aware path. Nullable for the same legacy
-- reason; required for any row that names a wallet (see paired CHECK).
ALTER TABLE token_ledger
  ADD COLUMN IF NOT EXISTS idempotency_key text;

-- External reference (e.g. WhatsApp wamid, wallet operation id). Optional.
ALTER TABLE token_ledger
  ADD COLUMN IF NOT EXISTS external_ref text;

-- New created_at distinct from the existing occurred_at: occurred_at is the
-- domain timestamp (when the business event happened); created_at is when
-- the row was persisted. Same value for synchronous writes, but kept
-- separate so reconciliation can distinguish "late writeback" cases.
ALTER TABLE token_ledger
  ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();

-- Paired CHECK: wallet_id and idempotency_key must be both NULL (legacy RLS
-- demo) or both NOT NULL (wallet-aware path). Prevents half-populated rows.
ALTER TABLE token_ledger
  DROP CONSTRAINT IF EXISTS token_ledger_wallet_idem_paired;
ALTER TABLE token_ledger
  ADD CONSTRAINT token_ledger_wallet_idem_paired
  CHECK ((wallet_id IS NULL) = (idempotency_key IS NULL));

-- Wallet-aware kinds must be one of the F30/F37 lifecycle states. The CHECK
-- is partial (only fires on wallet-aware rows) so legacy fixtures using
-- 'topup' / 'master_grant' / etc keep working.
ALTER TABLE token_ledger
  DROP CONSTRAINT IF EXISTS token_ledger_wallet_kind_check;
ALTER TABLE token_ledger
  ADD CONSTRAINT token_ledger_wallet_kind_check
  CHECK (wallet_id IS NULL OR kind IN ('reserve','commit','release','grant'));

-- Unique idempotency per wallet — the load-bearing guarantee for retried
-- LLM calls (F37). Partial so it does not collide with legacy NULL rows.
CREATE UNIQUE INDEX IF NOT EXISTS token_ledger_wallet_idem_idx
  ON token_ledger (wallet_id, idempotency_key)
  WHERE wallet_id IS NOT NULL;

-- Hot read path: "show me the most recent entries for this wallet".
CREATE INDEX IF NOT EXISTS token_ledger_wallet_created_idx
  ON token_ledger (wallet_id, created_at DESC)
  WHERE wallet_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- courtesy_grant: initial credit issued on tenant onboarding.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS courtesy_grant (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL UNIQUE,
  amount              bigint NOT NULL CHECK (amount > 0),
  granted_at          timestamptz NOT NULL DEFAULT now(),
  granted_by_user_id  uuid,
  note                text
);

ALTER TABLE courtesy_grant OWNER TO app_admin;

ALTER TABLE courtesy_grant ENABLE ROW LEVEL SECURITY;
ALTER TABLE courtesy_grant FORCE ROW LEVEL SECURITY;

-- Append-only from the runtime's perspective: a tenant can read its own
-- grant but never edits or deletes it. master_ops issues the grant during
-- tenant creation and may revoke it (with audit) if onboarding fails.
REVOKE ALL ON courtesy_grant FROM PUBLIC;
GRANT SELECT ON courtesy_grant TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON courtesy_grant TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON courtesy_grant;
CREATE POLICY tenant_isolation_select ON courtesy_grant
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS courtesy_grant_master_ops_audit ON courtesy_grant;
CREATE TRIGGER courtesy_grant_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON courtesy_grant
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
