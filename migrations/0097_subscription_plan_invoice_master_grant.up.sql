-- 0097_subscription_plan_invoice_master_grant.up.sql
-- Fase 2.5 C1 / SIN-62875: subscription + billing + master grants schema.
--
-- Six concerns ship together so the relationships between them are enforced
-- by the database from day one (see ADR-0097 and ADR-0098 — the ADRs are
-- canonical; plan-doc SIN-62195 §4 was superseded by ADR-0098 for the
-- master_grant schema specifically).
--
--   * plan                  — global catalogue of billable plans. No RLS, no
--                             audit trigger: it is a non-tenanted reference
--                             table the CEO/master operator curates via
--                             master_ops.
--   * subscription          — one row per tenant (partial UNIQUE on
--                             status='active'); RLS by tenant_id; audited.
--   * invoice               — append-mostly billing rows; UNIQUE(tenant_id,
--                             period_start) partial WHERE state ≠
--                             'cancelled_by_master' so a master cancellation
--                             does not block a fresh invoice for the same
--                             period. RLS by tenant_id; audited.
--   * master_grant          — internal UUID PK + external ULID
--                             (ADR-0098 §D1). Holds master-issued grants
--                             after they land (either directly or via the
--                             4-eyes promotion from master_grant_request).
--                             JSONB payload; reason / revoke_reason ≥ 10
--                             chars; consumed_ref TEXT NULL points at the
--                             downstream artifact (ledger entry or
--                             subscription external id) without an FK so
--                             the contexts stay decoupled. RLS by
--                             tenant_id; audited.
--   * master_grant_request  — 4-eyes staging for grants above the per-grant
--                             cap (ADR-0098 §D5). state='awaiting_approval'
--                             at insert; the second master fills
--                             requires_second_approver_id and transitions
--                             to 'approved' or 'rejected'. Only an approved
--                             request promotes to a master_grant row
--                             (handled by C8 writer, not here). master_ops
--                             only — no runtime SELECT grant.
--   * token_ledger          — extension only: NOT NULL `source` column with
--                             a temporary DEFAULT (expand→backfill→contract
--                             — the DEFAULT stays this migration and may be
--                             dropped in a later one once all writers
--                             supply `source`; see ADR-0097), plus an
--                             optional FK `master_grant_id` (UUID, matches
--                             the new master_grant PK type).
--
-- The plan-doc / issue body name the ledger table "ledger_entry"; the
-- actual table in this repo is `token_ledger` (created in 0003). The
-- migration extends that table.
--
-- Master user identity: ADR-0098 §D1 references `master_user(id)` as the
-- FK target. That table does not yet exist; the CRM convention (see
-- audit_log_security, master_session, master_mfa) is to FK against
-- `users(id)` with the `is_master` boolean filter at the domain layer.
-- We keep that convention here. When `master_user` is introduced in a
-- future migration, the FK target can be tightened without changing the
-- column type (UUID stays UUID).
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
-- master_grant: master-issued grants (free subscription period or extra
-- tokens). Internal UUID PK + external ULID (ADR-0098 §D1).
--
-- Schema notes (ADR-0098 §D1, retained here so a reader of the migration
-- understands the constraint semantics without bouncing to docs):
--
--   * `id` is an internal UUID — implementation detail of the row.
--   * `external_id` is the ULID (Crockford base32, 26 chars) that
--     appears in audit logs, master-UI URLs, support tickets, and the
--     `consumed_ref` of downstream artifacts. ULID lexicographic
--     ordering is the chronological sort key.
--   * `payload` is opaque JSONB — kind-specific (e.g. {"tokens": N} for
--     extra_tokens, {"months": N, "plan_id": "..."} for
--     free_subscription_period). Validation lives at the domain layer
--     per ADR-0098 §D6, not at the column level — keeps the schema
--     forward-compatible with future grant kinds (D7 reservation).
--   * `consumed_ref` is a TEXT pointer (no FK) so wallet and billing
--     contexts can write into master_grant without an inter-context FK.
--     Decoupling is intentional (ADR-0098 §D4 last paragraph).
--   * `master_grant_revoke_consistency` enforces: a revoked grant must
--     have revoked_at + revoked_by_user_id + revoke_reason (≥10 chars)
--     populated together AND consumed_at IS NULL. Defence-in-depth
--     against a buggy caller, malicious operator, or pg client mishap.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS master_grant (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  external_id         text NOT NULL UNIQUE,
  tenant_id           uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  kind                text NOT NULL CHECK (kind IN ('free_subscription_period','extra_tokens')),
  payload             jsonb NOT NULL,
  reason              text NOT NULL CHECK (char_length(reason) >= 10),
  created_by_user_id  uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at          timestamptz NOT NULL DEFAULT now(),
  consumed_at         timestamptz,
  consumed_ref        text,
  revoked_at          timestamptz,
  revoked_by_user_id  uuid REFERENCES users(id) ON DELETE RESTRICT,
  revoke_reason       text,

  -- A revocation requires revoked_at, revoked_by_user_id and
  -- revoke_reason (≥10 chars) populated together, and is only legal
  -- while the grant has not been consumed yet (ADR-0098 §D1, §D4).
  -- Once `consumed_at` is set, the grant becomes terminal: the only
  -- corrective path is a compensating grant (§D4 last paragraph).
  --
  -- The explicit IS NOT NULL guard prevents char_length(NULL) from
  -- propagating UNKNOWN through the OR chain — Postgres treats UNKNOWN
  -- on a CHECK as "does not violate", which would let revoked_at slip
  -- through without an attributable user / reason.
  CONSTRAINT master_grant_revoke_consistency CHECK (
    (revoked_at IS NULL
       AND revoked_by_user_id IS NULL
       AND revoke_reason IS NULL)
    OR
    (revoked_at IS NOT NULL
       AND revoked_by_user_id IS NOT NULL
       AND revoke_reason IS NOT NULL
       AND char_length(revoke_reason) >= 10
       AND consumed_at IS NULL)
  )
);

-- Tenanted history listing: ADR-0098 §D1 partial-index recipe.
CREATE INDEX IF NOT EXISTS master_grant_tenant_idx
  ON master_grant (tenant_id, created_at DESC);

-- Partial unconsumed-and-unrevoked index supports the master UI listing
-- "pending grants for tenant X" without a full table scan.
CREATE INDEX IF NOT EXISTS master_grant_unconsumed_idx
  ON master_grant (tenant_id)
  WHERE consumed_at IS NULL AND revoked_at IS NULL;

ALTER TABLE master_grant OWNER TO app_admin;

ALTER TABLE master_grant ENABLE ROW LEVEL SECURITY;
ALTER TABLE master_grant FORCE ROW LEVEL SECURITY;

REVOKE ALL ON master_grant FROM PUBLIC;
-- Tenants can read their own grants (so the manager UI can list courtesy
-- history). master_ops issues, revokes, and marks consumed.
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
-- master_grant_request: 4-eyes staging for over-cap grants (ADR-0098 §D5).
--
-- A grant above the per-grant cap (10M tokens-equivalent, ADR-0088 §D7)
-- is NOT inserted directly into master_grant. Instead it lands here with
-- state='awaiting_approval', no second-approver, no decision timestamp.
-- A second master then either approves (filling requires_second_approver_id
-- and decided_at, transitioning to 'approved') or rejects (same fields
-- filled, state='rejected'). Only an approved request promotes to a
-- master_grant row — that promotion is owned by the C8 writer
-- ([SIN-62883](/SIN/issues/SIN-62883)), not by this migration.
--
-- The approver MUST be a different master than the requester (CHECK at
-- the schema level + handler-level guard, defence in depth). The CHECK
-- only triggers when requires_second_approver_id is populated — the
-- awaiting_approval state has it NULL.
--
-- No runtime SELECT grant: requests are internal master plumbing, not
-- tenant-visible. RLS is enabled but no policy is attached for runtime,
-- so app_runtime fails closed (zero rows visible). app_master_ops has
-- BYPASSRLS in its role grant chain (see 0001 / app_master_ops setup).
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS master_grant_request (
  id                          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  external_id                 text NOT NULL UNIQUE,
  tenant_id                   uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  kind                        text NOT NULL
    CHECK (kind IN ('free_subscription_period','extra_tokens')),
  payload                     jsonb NOT NULL,
  reason                      text NOT NULL CHECK (char_length(reason) >= 10),
  created_by_user_id          uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  requires_second_approver_id uuid REFERENCES users(id) ON DELETE RESTRICT,
  state                       text NOT NULL
    CHECK (state IN ('awaiting_approval','approved','rejected')),
  decided_at                  timestamptz,
  created_at                  timestamptz NOT NULL DEFAULT now(),

  -- 4-eyes invariant: the approver cannot be the requester (ADR-0098
  -- §D5). NULL approver (awaiting_approval state) is fine.
  CONSTRAINT master_grant_request_distinct_approver CHECK (
    requires_second_approver_id IS NULL
    OR requires_second_approver_id <> created_by_user_id
  ),

  -- State machine consistency: awaiting_approval has no approver / no
  -- decision; approved or rejected MUST have both. NULL/NULL split
  -- here is what gates the C8 promoter from acting on an undecided
  -- request.
  CONSTRAINT master_grant_request_state_consistency CHECK (
    (state = 'awaiting_approval'
       AND requires_second_approver_id IS NULL
       AND decided_at IS NULL)
    OR
    (state IN ('approved','rejected')
       AND requires_second_approver_id IS NOT NULL
       AND decided_at IS NOT NULL)
  )
);

CREATE INDEX IF NOT EXISTS master_grant_request_tenant_idx
  ON master_grant_request (tenant_id, created_at DESC);

-- Partial index supports the master UI listing "requests awaiting my
-- approval" without scanning resolved decisions.
CREATE INDEX IF NOT EXISTS master_grant_request_awaiting_idx
  ON master_grant_request (created_at DESC)
  WHERE state = 'awaiting_approval';

ALTER TABLE master_grant_request OWNER TO app_admin;

ALTER TABLE master_grant_request ENABLE ROW LEVEL SECURITY;
ALTER TABLE master_grant_request FORCE ROW LEVEL SECURITY;

REVOKE ALL ON master_grant_request FROM PUBLIC;
-- master_ops only — no runtime visibility (requests are internal
-- master-side plumbing until they promote to a master_grant row).
GRANT SELECT, INSERT, UPDATE, DELETE ON master_grant_request TO app_master_ops;

DROP TRIGGER IF EXISTS master_grant_request_master_ops_audit ON master_grant_request;
CREATE TRIGGER master_grant_request_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON master_grant_request
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
-- Type matches the new master_grant.id (UUID, per ADR-0098 §D1).
ALTER TABLE token_ledger
  ADD COLUMN IF NOT EXISTS master_grant_id uuid REFERENCES master_grant(id);

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
