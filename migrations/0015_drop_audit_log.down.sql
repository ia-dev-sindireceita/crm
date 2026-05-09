-- 0015_drop_audit_log.down.sql
-- SIN-62424: restore the legacy `audit_log` table and the legacy
-- `app_audit` INSERT grant on it, so the environment matches its
-- pre-Phase-B.2 shape. This is a forensic rollback path: it does NOT
-- migrate writes from `audit_log_security` / `audit_log_data` back
-- into `audit_log` — the table comes back empty.
--
-- The schema mirrors 0007_create_audit_log.up.sql (table, indexes,
-- RLS, trigger) plus the 0009_app_audit_role.up.sql grant
-- (`GRANT INSERT ON audit_log TO app_audit`). Anything that 0007 / 0009
-- wired up MUST be wired back up here so the rollback is real.
--
-- Run as app_admin. Idempotent (CREATE IF NOT EXISTS, DROP/CREATE
-- POLICY, DROP/CREATE TRIGGER).

BEGIN;

CREATE TABLE IF NOT EXISTS audit_log (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  actor_user_id   uuid REFERENCES users(id) ON DELETE SET NULL,
  event           text NOT NULL,
  target          jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS audit_log_tenant_id_created_at_idx
  ON audit_log (tenant_id, created_at);

ALTER TABLE audit_log OWNER TO app_admin;

ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;

REVOKE ALL ON audit_log FROM PUBLIC;
GRANT SELECT, INSERT ON audit_log TO app_runtime;
GRANT SELECT, INSERT ON audit_log TO app_master_ops;
REVOKE UPDATE, DELETE ON audit_log FROM app_runtime;
REVOKE UPDATE, DELETE ON audit_log FROM app_master_ops;

-- Re-apply 0009's least-privilege grant (the role itself still exists;
-- only the grant was lost when DROP TABLE cascaded in 0015 up).
REVOKE ALL ON audit_log FROM app_audit;
GRANT INSERT ON audit_log TO app_audit;

DROP POLICY IF EXISTS tenant_isolation_select ON audit_log;
CREATE POLICY tenant_isolation_select ON audit_log
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON audit_log;
CREATE POLICY tenant_isolation_insert ON audit_log
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS audit_log_master_ops_audit ON audit_log;
CREATE TRIGGER audit_log_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON audit_log
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
