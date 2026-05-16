-- 0099_ai_policy_audit.up.sql
-- Fase 3 H1 / SIN-62353 (decisão #8): per-field audit ledger for ai_policy
-- mutations.
--
-- Every write to ai_policy (insert, update, soft-delete) emits one row per
-- changed field via the aipolicy.AuditLogger port (decorator over the
-- Repository). The table is append-only by grant (REVOKE UPDATE, DELETE
-- from app_runtime), tenant-scoped by RLS, and pruned by the existing
-- LGPD purge job using tenants.audit_data_retention_months (12-month
-- default per migration 0084, configurable 6–60 by tenant).
--
-- Why a dedicated table instead of extending audit_log_security or
-- audit_log_data:
--
--   * audit_log_security wire vocabulary is closed (CHECK in migration
--     0083 + 0091); ai_policy mutations are configuration changes, not
--     identity/authn events.
--   * audit_log_data is for PII access, not configuration. Mixing
--     config changes there would distort the LGPD purge math.
--   * The product brief asks for a per-tenant pop-out view + a master
--     console slice. A dedicated table keeps the query plan trivial
--     and lets the indexer give us O(log n) cursor pagination.
--
-- Columns mirror the SIN-62353 spec:
--   * scope_kind ∈ {tenant, team, channel}, identical to ai_policy.
--   * scope_id text — short channel keys ('whatsapp', 'instagram') and
--     uuid-string tenant/team ids share the column shape.
--   * field is the ai_policy column name that changed (one row per
--     changed field). For lifecycle events without a column changeset
--     we use synthetic field names ('__created__', '__deleted__').
--   * old_value/new_value are jsonb so the resolver can keep typed
--     payloads (booleans, strings) without per-type columns.
--   * actor_user_id is the human user (master or tenant operator) who
--     authored the change; nullable because system migrations or back-
--     fills do not have a user actor.
--   * actor_master flags whether the change happened inside a master
--     impersonation session — the visualization tints those rows red
--     so tenant admins can spot master-driven mutations at a glance.
--
-- Run as app_admin. Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS ai_policy_audit (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  scope_kind      text NOT NULL
                    CHECK (scope_kind IN ('tenant','team','channel')),
  scope_id        text NOT NULL,
  field           text NOT NULL,
  old_value       jsonb NOT NULL DEFAULT 'null'::jsonb,
  new_value       jsonb NOT NULL DEFAULT 'null'::jsonb,
  actor_user_id   uuid REFERENCES users(id) ON DELETE SET NULL,
  actor_master    boolean NOT NULL DEFAULT false,
  created_at      timestamptz NOT NULL DEFAULT now()
);

-- Cursor pagination index: (created_at DESC, id DESC) is the keyset
-- the list handler scans with WHERE (created_at, id) < ($cursor_ts,
-- $cursor_id) LIMIT $page. Tenant-scoped first because RLS filters by
-- tenant; the planner can satisfy the tenant predicate from the
-- predicate first and then walk the sorted suffix.
CREATE INDEX IF NOT EXISTS ai_policy_audit_tenant_created_idx
  ON ai_policy_audit (tenant_id, created_at DESC, id DESC);

-- Scope-filter index: the tenant admin view lets users filter by a
-- specific (scope_kind, scope_id) pair (e.g. "what changed on the
-- whatsapp channel last week"). Composite index satisfies that path
-- without forcing the planner back to the tenant_created index.
CREATE INDEX IF NOT EXISTS ai_policy_audit_tenant_scope_created_idx
  ON ai_policy_audit (tenant_id, scope_kind, scope_id, created_at DESC);

ALTER TABLE ai_policy_audit OWNER TO app_admin;
ALTER TABLE ai_policy_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_policy_audit FORCE ROW LEVEL SECURITY;

-- Append-only by grant: the runtime can SELECT (for the admin views)
-- and INSERT (for the AuditLogger port), but never UPDATE or DELETE.
-- DELETE is reserved for app_master_ops so the LGPD purge job can
-- sweep expired rows; UPDATE remains denied to everyone including
-- master_ops because the wire shape is "append a corrective row" not
-- "rewrite history".
REVOKE ALL ON ai_policy_audit FROM PUBLIC;
GRANT SELECT, INSERT ON ai_policy_audit TO app_runtime;
GRANT SELECT, INSERT, DELETE ON ai_policy_audit TO app_master_ops;
REVOKE UPDATE ON ai_policy_audit FROM app_runtime;
REVOKE UPDATE ON ai_policy_audit FROM app_master_ops;

-- Tenant isolation: SELECT and INSERT are tenant-scoped via the
-- standard app.tenant_id GUC; DELETE is master-only and ignores the
-- GUC because the purge job sweeps every tenant in one transaction
-- (the master_ops audit ledger captures the cross-tenant sweep).
DROP POLICY IF EXISTS tenant_isolation_select ON ai_policy_audit;
CREATE POLICY tenant_isolation_select ON ai_policy_audit
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON ai_policy_audit;
CREATE POLICY tenant_isolation_insert ON ai_policy_audit
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS master_ops_full ON ai_policy_audit;
CREATE POLICY master_ops_full ON ai_policy_audit
  FOR ALL TO app_master_ops
  USING (true)
  WITH CHECK (true);

COMMIT;
