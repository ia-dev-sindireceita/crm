-- 0097_subscription_plan_invoice_master_grant.up.sql
-- Fase 2.5 C1 / SIN-62875: subscription + billing + master grants schema.
--
-- Five concerns ship together so the relationships between them are enforced
-- by the database from day one (see plan-doc SIN-62195 §4 and ADR-0097/0098):
--
--   * plan           — global catalogue of billable plans. No RLS, no audit
--                      trigger: it is a non-tenanted reference table the
--                      CEO/master operator curates via master_ops.
--   * subscription   — one row per tenant (partial UNIQUE on
--                      status='active'); RLS by tenant_id; master_ops_audit.
--   * invoice        — append-mostly billing rows; UNIQUE(tenant_id,
--                      period_start) partial WHERE state ≠ 'cancelled_by_master'
--                      so a master cancellation does not block a fresh
--                      invoice for the same period. RLS by tenant_id;
--                      master_ops_audit.
--   * master_grant   — ULID PK supplied by the application (text, not uuid)
--                      so the master endpoint can return the grant id
--                      synchronously without a round-trip. tenanted but
--                      master-issued; RLS by tenant_id; master_ops_audit.
--   * token_ledger   — extension only: NOT NULL `source` column with a
--                      temporary DEFAULT (expand→backfill→contract — the
--                      DEFAULT stays in place this migration and is dropped
--                      in the next one only if needed; see ADR-0097),
--                      plus an optional FK `master_grant_id`.
--
-- The plan-doc / issue body name the ledger table "ledger_entry"; the
-- actual table in this repo is `token_ledger` (created in 0003). The
-- migration extends that table.
--
-- Run as app_admin (BYPASSRLS=true required to attach policies and grants).
-- Idempotent: CREATE TABLE IF NOT EXISTS, ADD COLUMN IF NOT EXISTS, DROP
-- CONSTRAINT IF EXISTS, etc.

BEGIN;

-- ---------------------------------------------------------------------------
-- plan: global catalogue. No RLS (catálogo).
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS plan (
  id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug                 text NOT NULL UNIQUE,
  name                 text NOT NULL,
  price_cents_brl      integer NOT NULL CHECK (price_cents_brl >= 0),
  monthly_token_quota  bigint NOT NULL CHECK (monthly_token_quota >= 0),
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE plan OWNER TO app_admin;

REVOKE ALL ON plan FROM PUBLIC;
-- Tenants need to read plan names/quotas (e.g. to render their billing
-- page) but cannot mutate. master_ops curates the catalogue.
GRANT SELECT ON plan TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON plan TO app_master_ops;

-- ---------------------------------------------------------------------------
-- subscription: one active subscription per tenant.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS subscription (
  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id             uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  plan_id               uuid NOT NULL REFERENCES plan(id),
  status                text NOT NULL CHECK (status IN ('active','cancelled')),
  current_period_start  timestamptz NOT NULL,
  current_period_end    timestamptz NOT NULL,
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT subscription_period_order CHECK (current_period_end > current_period_start)
);

-- One ACTIVE subscription per tenant. A cancelled row stays in place for
-- audit; a fresh active subscription can be created next to it.
CREATE UNIQUE INDEX IF NOT EXISTS subscription_one_active_per_tenant_idx
  ON subscription (tenant_id)
  WHERE status = 'active';

CREATE INDEX IF NOT EXISTS subscription_tenant_id_idx
  ON subscription (tenant_id);

ALTER TABLE subscription OWNER TO app_admin;

ALTER TABLE subscription ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription FORCE ROW LEVEL SECURITY;

REVOKE ALL ON subscription FROM PUBLIC;
-- Tenants read their own subscription. Writes go through master_ops
-- (plan assignment is a master action; see ADR-0090 RBAC matrix).
GRANT SELECT ON subscription TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON subscription TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON subscription;
CREATE POLICY tenant_isolation_select ON subscription
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS subscription_master_ops_audit ON subscription;
CREATE TRIGGER subscription_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON subscription
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- invoice: monthly invoices per subscription.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS invoice (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  subscription_id     uuid NOT NULL REFERENCES subscription(id),
  period_start        date NOT NULL,
  period_end          date NOT NULL,
  amount_cents_brl    integer NOT NULL CHECK (amount_cents_brl >= 0),
  state               text NOT NULL CHECK (state IN ('pending','paid','cancelled_by_master')),
  cancelled_reason    text,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT invoice_period_order CHECK (period_end > period_start),
  -- A cancellation requires a human-readable reason of at least 10
  -- characters; non-cancelled rows MUST NOT carry a stray reason. Keeps
  -- the audit trail honest for ADR-0098 / SecurityEngineer review (C7).
  -- The explicit IS NOT NULL guard avoids the SQL UNKNOWN trap:
  -- char_length(NULL) is NULL, NULL >= 10 is NULL, and a NULL branch
  -- inside an OR makes the whole CHECK pass — undermining the rule.
  CONSTRAINT invoice_cancelled_reason_required CHECK (
    (state = 'cancelled_by_master'
       AND cancelled_reason IS NOT NULL
       AND char_length(cancelled_reason) >= 10)
    OR
    (state <> 'cancelled_by_master' AND cancelled_reason IS NULL)
  )
);

-- Idempotency: the renewer MAY rerun within a single day. The partial
-- UNIQUE allows a fresh pending/paid invoice for a period that was
-- previously cancelled by master (plan-doc §3 / CA #6).
CREATE UNIQUE INDEX IF NOT EXISTS invoice_tenant_period_active_idx
  ON invoice (tenant_id, period_start)
  WHERE state <> 'cancelled_by_master';

CREATE INDEX IF NOT EXISTS invoice_tenant_id_idx
  ON invoice (tenant_id);

CREATE INDEX IF NOT EXISTS invoice_subscription_id_idx
  ON invoice (subscription_id);

ALTER TABLE invoice OWNER TO app_admin;

ALTER TABLE invoice ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice FORCE ROW LEVEL SECURITY;

REVOKE ALL ON invoice FROM PUBLIC;
-- Tenants read their own invoices. Writes go through master_ops (the
-- renewer runs as master_ops; manual paid/cancel transitions are master
-- actions).
GRANT SELECT ON invoice TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON invoice TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON invoice;
CREATE POLICY tenant_isolation_select ON invoice
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS invoice_master_ops_audit ON invoice;
CREATE TRIGGER invoice_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON invoice
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- master_grant: master-issued courtesy grants (free subscription period
-- or extra tokens). ULID PK supplied by the application.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS master_grant (
  id                  text PRIMARY KEY,
  tenant_id           uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  created_by_user_id  text NOT NULL,
  kind                text NOT NULL CHECK (kind IN ('free_subscription_period','extra_tokens')),
  reason              text NOT NULL CHECK (char_length(reason) >= 10),
  amount              bigint,
  period_days         integer,
  created_at          timestamptz NOT NULL DEFAULT now(),
  revoked_at          timestamptz,
  revoked_reason      text,
  consumed_at         timestamptz,
  -- kind drives which payload column is required. extra_tokens carries
  -- `amount`; free_subscription_period carries `period_days`. Both must
  -- be > 0; the other column must be NULL. Mirrors ADR-0098.
  CONSTRAINT master_grant_payload_for_kind CHECK (
    (kind = 'extra_tokens'
       AND amount IS NOT NULL AND amount > 0
       AND period_days IS NULL)
    OR
    (kind = 'free_subscription_period'
       AND period_days IS NOT NULL AND period_days > 0
       AND amount IS NULL)
  ),
  -- A revocation requires both timestamp and reason (≥10 chars), and is
  -- only legal while the grant has not been consumed yet (ADR-0098).
  -- Once `consumed_at` is set, `revoked_at` is permanently NULL.
  -- The explicit IS NOT NULL guard prevents char_length(NULL) from
  -- propagating UNKNOWN through the OR chain — Postgres treats UNKNOWN
  -- on a CHECK as "does not violate", which would let revoked_at slip
  -- through without a reason.
  CONSTRAINT master_grant_revocation_consistent CHECK (
    (revoked_at IS NULL AND revoked_reason IS NULL)
    OR
    (revoked_at IS NOT NULL
       AND revoked_reason IS NOT NULL
       AND char_length(revoked_reason) >= 10
       AND consumed_at IS NULL)
  )
);

CREATE INDEX IF NOT EXISTS master_grant_tenant_id_idx
  ON master_grant (tenant_id);

ALTER TABLE master_grant OWNER TO app_admin;

ALTER TABLE master_grant ENABLE ROW LEVEL SECURITY;
ALTER TABLE master_grant FORCE ROW LEVEL SECURITY;

REVOKE ALL ON master_grant FROM PUBLIC;
-- Tenants can read their own grants (so the manager UI can list courtesy
-- history). master_ops issues and revokes grants.
GRANT SELECT ON master_grant TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON master_grant TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON master_grant;
CREATE POLICY tenant_isolation_select ON master_grant
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS master_grant_master_ops_audit ON master_grant;
CREATE TRIGGER master_grant_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON master_grant
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- token_ledger: extend the wallet-aware journal with source attribution
-- and an optional FK back to master_grant (when source='master_grant').
-- ---------------------------------------------------------------------------

-- Step 1 (expand): add the column with a temporary default so existing
-- rows can be backfilled in the same statement. The default labels
-- legacy entries as 'consumption' — every wallet-aware kind written so
-- far (reserve/commit/release/grant) is a consumption-style movement;
-- the 0089 RLS-demo legacy rows (NULL wallet_id) are also accepting of
-- the 'consumption' label because they predate the source taxonomy.
-- Step 2 in a follow-up migration MAY drop the default once all writes
-- supply `source` explicitly.
ALTER TABLE token_ledger
  ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT 'consumption';

ALTER TABLE token_ledger
  DROP CONSTRAINT IF EXISTS token_ledger_source_check;
ALTER TABLE token_ledger
  ADD CONSTRAINT token_ledger_source_check
  CHECK (source IN ('monthly_alloc','master_grant','consumption'));

-- Optional FK to master_grant. NULL for monthly_alloc and consumption
-- entries; REQUIRED when source='master_grant' (paired CHECK below).
ALTER TABLE token_ledger
  ADD COLUMN IF NOT EXISTS master_grant_id text REFERENCES master_grant(id);

ALTER TABLE token_ledger
  DROP CONSTRAINT IF EXISTS token_ledger_master_grant_pairing;
ALTER TABLE token_ledger
  ADD CONSTRAINT token_ledger_master_grant_pairing
  CHECK (
    (source = 'master_grant' AND master_grant_id IS NOT NULL)
    OR
    (source <> 'master_grant' AND master_grant_id IS NULL)
  );

CREATE INDEX IF NOT EXISTS token_ledger_master_grant_id_idx
  ON token_ledger (master_grant_id)
  WHERE master_grant_id IS NOT NULL;

COMMIT;
