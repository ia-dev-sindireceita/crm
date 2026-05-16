-- 0098_ai_policy_ai_summary_product_argument.up.sql
-- Fase 3 W1A / SIN-62900: schema foundation for the IA wave.
--
-- Four tables ship together because they form the data layer the rest of
-- Fase 3 (W2A/W2B/W2C, W3*, W4*) builds on. See plan in
-- /SIN/issues/SIN-62196#document-plan and ADRs 0040-0043 (W1B).
--
--   * ai_policy        — per-scope (tenant/team/channel) IA configuration.
--                        Cascade resolver in W2A is canal>equipe>tenant. RLS
--                        by tenant. UNIQUE (tenant_id, scope_type, scope_id)
--                        gives the resolver an indexed lookup and rejects
--                        accidental duplicate scope rows.
--   * ai_summary       — generated conversation summaries with TTL. RLS by
--                        tenant; conversation_id index supports the hot
--                        "latest summary for this conversation" path.
--                        FK conversation_id ON DELETE CASCADE keeps the
--                        summary lifetime ≤ conversation lifetime.
--   * product          — per-tenant catalog of billable items. RLS by
--                        tenant. tags text[] gives the W2B resolver a
--                        cheap filter without pulling in a join table.
--   * product_argument — per-scope selling argument attached to a product.
--                        Cascade resolver in W2B mirrors ai_policy.
--                        FK product_id ON DELETE CASCADE so removing a
--                        product clears its arguments. RLS by tenant.
--
-- Scope shape:
--   * scope_type ∈ ('tenant','team','channel') — CHECK constraint.
--   * scope_id is `text` because:
--       - channel keys are short identifiers (e.g. 'whatsapp',
--         'instagram') — not UUIDs.
--       - team / tenant ids are UUIDs that the resolver always passes as
--         their string form.
--     Keeping the column as text lets one row shape serve every scope
--     type without union-tricks or per-kind tables.
--
-- Run as app_admin (BYPASSRLS=true required to attach policies and grants).
-- Idempotent: CREATE TABLE IF NOT EXISTS, DROP POLICY IF EXISTS, etc.

BEGIN;

-- ---------------------------------------------------------------------------
-- ai_policy
-- ai_enabled / opt_in default to false so a freshly-onboarded tenant has
-- IA OFF until the tenant explicitly opts in (LGPD posture, ADR-0041).
-- anonymize defaults to true: even when IA is enabled, payloads sent
-- upstream are anonymized by default (ADR-0041, decisão #8).
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS ai_policy (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  scope_type      text NOT NULL
                    CHECK (scope_type IN ('tenant','team','channel')),
  scope_id        text NOT NULL,
  model           text NOT NULL DEFAULT 'openrouter/auto',
  prompt_version  text NOT NULL DEFAULT 'v1',
  tone            text NOT NULL DEFAULT 'neutro',
  language        text NOT NULL DEFAULT 'pt-BR',
  ai_enabled      boolean NOT NULL DEFAULT false,
  anonymize       boolean NOT NULL DEFAULT true,
  opt_in          boolean NOT NULL DEFAULT false,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ai_policy_tenant_scope_uniq
    UNIQUE (tenant_id, scope_type, scope_id)
);

CREATE INDEX IF NOT EXISTS ai_policy_tenant_id_idx
  ON ai_policy (tenant_id);

ALTER TABLE ai_policy OWNER TO app_admin;
ALTER TABLE ai_policy ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_policy FORCE ROW LEVEL SECURITY;

REVOKE ALL ON ai_policy FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_policy TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_policy TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON ai_policy;
CREATE POLICY tenant_isolation_select ON ai_policy
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON ai_policy;
CREATE POLICY tenant_isolation_insert ON ai_policy
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON ai_policy;
CREATE POLICY tenant_isolation_update ON ai_policy
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON ai_policy;
CREATE POLICY tenant_isolation_delete ON ai_policy
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS ai_policy_master_ops_audit ON ai_policy;
CREATE TRIGGER ai_policy_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON ai_policy
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- ai_summary
-- expires_at is nullable: a NULL means "no TTL" (rare; opt-in for tenants
-- that explicitly disable expiration). invalidated_at is filled when a
-- newer message renders the summary stale — the resolver treats any row
-- with invalidated_at IS NOT NULL or expires_at < now() as cache-miss.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS ai_summary (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  conversation_id uuid NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
  summary_text    text NOT NULL,
  model           text NOT NULL,
  tokens_in       integer NOT NULL CHECK (tokens_in >= 0),
  tokens_out      integer NOT NULL CHECK (tokens_out >= 0),
  generated_at    timestamptz NOT NULL DEFAULT now(),
  expires_at      timestamptz,
  invalidated_at  timestamptz
);

CREATE INDEX IF NOT EXISTS ai_summary_tenant_id_idx
  ON ai_summary (tenant_id);

-- Hot path: "latest still-valid summary for conversation X". The partial
-- predicate lets the planner skip invalidated rows; generated_at DESC
-- gives the resolver an index scan for the most recent entry.
CREATE INDEX IF NOT EXISTS ai_summary_conversation_id_idx
  ON ai_summary (conversation_id, generated_at DESC)
  WHERE invalidated_at IS NULL;

ALTER TABLE ai_summary OWNER TO app_admin;
ALTER TABLE ai_summary ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_summary FORCE ROW LEVEL SECURITY;

REVOKE ALL ON ai_summary FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_summary TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_summary TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON ai_summary;
CREATE POLICY tenant_isolation_select ON ai_summary
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON ai_summary;
CREATE POLICY tenant_isolation_insert ON ai_summary
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON ai_summary;
CREATE POLICY tenant_isolation_update ON ai_summary
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON ai_summary;
CREATE POLICY tenant_isolation_delete ON ai_summary
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS ai_summary_master_ops_audit ON ai_summary;
CREATE TRIGGER ai_summary_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON ai_summary
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- product
-- Per-tenant catalogue. tags text[] is a denormalized list of free-form
-- labels; the W2B resolver filters by ANY($1) on this column, which the
-- planner can satisfy with a GIN index later if tag-cardinality grows.
-- For now the table starts unindexed on `tags` — adding the index in a
-- follow-up migration is cheap (CREATE INDEX CONCURRENTLY).
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS product (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name         text NOT NULL,
  description  text NOT NULL DEFAULT '',
  price_cents  integer NOT NULL DEFAULT 0 CHECK (price_cents >= 0),
  tags         text[] NOT NULL DEFAULT '{}',
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS product_tenant_id_idx
  ON product (tenant_id);

ALTER TABLE product OWNER TO app_admin;
ALTER TABLE product ENABLE ROW LEVEL SECURITY;
ALTER TABLE product FORCE ROW LEVEL SECURITY;

REVOKE ALL ON product FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON product TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON product TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON product;
CREATE POLICY tenant_isolation_select ON product
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON product;
CREATE POLICY tenant_isolation_insert ON product
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON product;
CREATE POLICY tenant_isolation_update ON product
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON product;
CREATE POLICY tenant_isolation_delete ON product
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS product_master_ops_audit ON product;
CREATE TRIGGER product_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON product
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- product_argument
-- Per-scope selling argument for a product. Same scope cascade shape as
-- ai_policy. UNIQUE (tenant_id, product_id, scope_type, scope_id) keeps
-- the resolver's "one argument per (product, scope)" invariant; the
-- W2B resolver picks the most specific match (channel > team > tenant).
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS product_argument (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  product_id      uuid NOT NULL REFERENCES product(id) ON DELETE CASCADE,
  scope_type      text NOT NULL
                    CHECK (scope_type IN ('tenant','team','channel')),
  scope_id        text NOT NULL,
  argument_text   text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT product_argument_product_scope_uniq
    UNIQUE (tenant_id, product_id, scope_type, scope_id)
);

CREATE INDEX IF NOT EXISTS product_argument_tenant_id_idx
  ON product_argument (tenant_id);
CREATE INDEX IF NOT EXISTS product_argument_product_id_idx
  ON product_argument (product_id);

ALTER TABLE product_argument OWNER TO app_admin;
ALTER TABLE product_argument ENABLE ROW LEVEL SECURITY;
ALTER TABLE product_argument FORCE ROW LEVEL SECURITY;

REVOKE ALL ON product_argument FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON product_argument TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON product_argument TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON product_argument;
CREATE POLICY tenant_isolation_select ON product_argument
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON product_argument;
CREATE POLICY tenant_isolation_insert ON product_argument
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON product_argument;
CREATE POLICY tenant_isolation_update ON product_argument
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON product_argument;
CREATE POLICY tenant_isolation_delete ON product_argument
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS product_argument_master_ops_audit ON product_argument;
CREATE TRIGGER product_argument_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON product_argument
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
