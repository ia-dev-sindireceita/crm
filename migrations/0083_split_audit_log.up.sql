-- 0083_split_audit_log.up.sql (re-landed from legacy 0012 per ADR 0086)
-- SIN-62252 / ADR 0004 §4: split the single per-tenant audit_log into
-- two append-only ledgers with different retention policies.
--
--   * audit_log_security — high-value security/identity events. Default
--     retention 24 months (configurable, never below 24m). NEVER touched
--     by the LGPD purge job. Permits tenant_id IS NULL rows so that
--     super-admin master events (login, master_grant, key_rotation,
--     impersonation_start/stop without an active tenant scope) have a
--     home that is not bound to any tenant lifecycle.
--   * audit_log_data — PII/data-access events. Default retention 12
--     months; per-tenant override via tenants.audit_data_retention_months
--     (added in migration 0013). The LGPD purge job sweeps this table
--     and only this table.
--
-- This migration creates the two new tables, indices, RLS policies, and
-- grants. It does NOT drop the legacy `audit_log` table — that drop is
-- gated on the legacy adapter (internal/adapter/db/postgres/audit_logger.go)
-- being migrated to the split writer (SIN-62252 follow-up). Keeping
-- `audit_log` for now lets the existing tests for the legacy writer
-- keep passing while the new path is wired up.
--
-- Run as app_admin. Idempotent.

BEGIN;

-- ---------------------------------------------------------------------------
-- audit_log_security
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS audit_log_security (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid REFERENCES tenants(id) ON DELETE CASCADE,
  actor_user_id   uuid REFERENCES users(id) ON DELETE SET NULL,
  event_type      text NOT NULL CHECK (event_type IN (
    'login',
    'login_fail',
    '2fa_enroll',
    '2fa_verify',
    'role_change',
    'impersonation_start',
    'impersonation_stop',
    'master_grant',
    'authz_deny',
    'signature_fail',
    'key_rotation'
  )),
  target          jsonb NOT NULL DEFAULT '{}'::jsonb,
  occurred_at     timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE audit_log_security OWNER TO app_admin;

CREATE INDEX IF NOT EXISTS audit_log_security_tenant_occurred_idx
  ON audit_log_security (tenant_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS audit_log_security_actor_occurred_idx
  ON audit_log_security (actor_user_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS audit_log_security_event_occurred_idx
  ON audit_log_security (event_type, occurred_at DESC);

ALTER TABLE audit_log_security ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log_security FORCE ROW LEVEL SECURITY;

REVOKE ALL ON audit_log_security FROM PUBLIC;
GRANT SELECT, INSERT ON audit_log_security TO app_runtime;
GRANT SELECT, INSERT ON audit_log_security TO app_master_ops;
REVOKE UPDATE, DELETE ON audit_log_security FROM app_runtime;
REVOKE UPDATE, DELETE ON audit_log_security FROM app_master_ops;

-- app_runtime: tenant-scoped reads/writes. The OR branch lets the
-- runtime emit master-context events (tenant_id IS NULL) when running
-- under impersonation/master flows that resolved to current_user =
-- app_master_ops via SET ROLE; in normal request handling current_user
-- stays app_runtime and only the tenant-scoped branch matches.
DROP POLICY IF EXISTS tenant_isolation_select ON audit_log_security;
CREATE POLICY tenant_isolation_select ON audit_log_security
  FOR SELECT TO app_runtime
  USING (
    tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
  );

DROP POLICY IF EXISTS tenant_isolation_insert ON audit_log_security;
CREATE POLICY tenant_isolation_insert ON audit_log_security
  FOR INSERT TO app_runtime
  WITH CHECK (
    tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
  );

-- app_master_ops sees and writes everything (cross-tenant + NULL).
DROP POLICY IF EXISTS master_ops_select ON audit_log_security;
CREATE POLICY master_ops_select ON audit_log_security
  FOR SELECT TO app_master_ops
  USING (true);

DROP POLICY IF EXISTS master_ops_insert ON audit_log_security;
CREATE POLICY master_ops_insert ON audit_log_security
  FOR INSERT TO app_master_ops
  WITH CHECK (true);

DROP TRIGGER IF EXISTS audit_log_security_master_ops_audit ON audit_log_security;
CREATE TRIGGER audit_log_security_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON audit_log_security
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- audit_log_data
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS audit_log_data (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  actor_user_id   uuid REFERENCES users(id) ON DELETE SET NULL,
  event_type      text NOT NULL CHECK (event_type IN (
    'read_pii',
    'write_contact',
    'export_csv',
    'lgpd_export',
    'lgpd_forget'
  )),
  target          jsonb NOT NULL DEFAULT '{}'::jsonb,
  occurred_at     timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE audit_log_data OWNER TO app_admin;

CREATE INDEX IF NOT EXISTS audit_log_data_tenant_occurred_idx
  ON audit_log_data (tenant_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS audit_log_data_actor_occurred_idx
  ON audit_log_data (actor_user_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS audit_log_data_event_occurred_idx
  ON audit_log_data (event_type, occurred_at DESC);

ALTER TABLE audit_log_data ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log_data FORCE ROW LEVEL SECURITY;

REVOKE ALL ON audit_log_data FROM PUBLIC;
GRANT SELECT, INSERT ON audit_log_data TO app_runtime;
GRANT SELECT, INSERT ON audit_log_data TO app_master_ops;
-- LGPD purge requires DELETE on this table. The purge runs as
-- app_master_ops (cross-tenant, audited via master_ops_audit_trigger).
GRANT DELETE ON audit_log_data TO app_master_ops;
REVOKE UPDATE, DELETE ON audit_log_data FROM app_runtime;
REVOKE UPDATE ON audit_log_data FROM app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON audit_log_data;
CREATE POLICY tenant_isolation_select ON audit_log_data
  FOR SELECT TO app_runtime
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON audit_log_data;
CREATE POLICY tenant_isolation_insert ON audit_log_data
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS master_ops_select ON audit_log_data;
CREATE POLICY master_ops_select ON audit_log_data
  FOR SELECT TO app_master_ops
  USING (true);

DROP POLICY IF EXISTS master_ops_insert ON audit_log_data;
CREATE POLICY master_ops_insert ON audit_log_data
  FOR INSERT TO app_master_ops
  WITH CHECK (true);

DROP POLICY IF EXISTS master_ops_delete ON audit_log_data;
CREATE POLICY master_ops_delete ON audit_log_data
  FOR DELETE TO app_master_ops
  USING (true);

DROP TRIGGER IF EXISTS audit_log_data_master_ops_audit ON audit_log_data;
CREATE TRIGGER audit_log_data_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON audit_log_data
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- audit_log_unified — read-only view for the master panel.
-- UNION ALL is cheap on append-only tables and lets the master UI use
-- one query for both ledgers. Discriminator column `source` lets the
-- panel filter without re-querying.
-- ---------------------------------------------------------------------------

DROP VIEW IF EXISTS audit_log_unified;
CREATE VIEW audit_log_unified AS
  SELECT
    'security'::text  AS source,
    id,
    tenant_id,
    actor_user_id,
    event_type,
    target,
    occurred_at
  FROM audit_log_security
  UNION ALL
  SELECT
    'data'::text      AS source,
    id,
    tenant_id,
    actor_user_id,
    event_type,
    target,
    occurred_at
  FROM audit_log_data;

ALTER VIEW audit_log_unified OWNER TO app_admin;
REVOKE ALL ON audit_log_unified FROM PUBLIC;
GRANT SELECT ON audit_log_unified TO app_master_ops;

COMMIT;
