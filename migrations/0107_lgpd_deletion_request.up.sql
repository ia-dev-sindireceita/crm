-- 0107_lgpd_deletion_request.up.sql
-- SIN-63186 / Fase 6 PR3: persistent tombstone for LGPD article-18
-- erasure requests. The web handler (POST /admin/lgpd/delete) writes
-- one row per (tenant_id, contact_id) request; the
-- lgpd-retention-purge-worker scans rows whose retention_until <= now()
-- and finalises the purge (drops fiscal/billing rows kept for
-- LGPD_FISCAL_RETENTION_YEARS, anonymises the contact row, removes any
-- residual non-fiscal data).
--
-- Idempotency contract: a second POST for the same contact while a
-- row exists in 'pending' status SHALL update only the most recent
-- request's metadata (justification, requested_by_user_id) and reset
-- retention_until. The handler enforces this via ON CONFLICT
-- (tenant_id, contact_id) WHERE status='pending'.
--
-- Run as app_admin. Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS lgpd_deletion_request (
  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id             uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  contact_id            uuid NOT NULL,
  requested_by_user_id  uuid REFERENCES users(id) ON DELETE SET NULL,
  justification         text NOT NULL DEFAULT '',
  status                text NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending', 'completed', 'failed')),
  retention_until       timestamptz NOT NULL,
  completed_at          timestamptz,
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE lgpd_deletion_request OWNER TO app_admin;

-- Index supports both the worker's "ready for purge" sweep and the
-- per-tenant idempotency check.
CREATE INDEX IF NOT EXISTS lgpd_deletion_request_retention_idx
  ON lgpd_deletion_request (status, retention_until);
CREATE INDEX IF NOT EXISTS lgpd_deletion_request_tenant_contact_idx
  ON lgpd_deletion_request (tenant_id, contact_id);

-- Idempotency: at most one pending request per (tenant, contact).
-- Completed requests are kept for audit; new requests after completion
-- are allowed (and create a fresh row).
CREATE UNIQUE INDEX IF NOT EXISTS lgpd_deletion_request_pending_uniq
  ON lgpd_deletion_request (tenant_id, contact_id)
  WHERE status = 'pending';

ALTER TABLE lgpd_deletion_request ENABLE ROW LEVEL SECURITY;
ALTER TABLE lgpd_deletion_request FORCE ROW LEVEL SECURITY;

REVOKE ALL ON lgpd_deletion_request FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON lgpd_deletion_request TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON lgpd_deletion_request TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON lgpd_deletion_request;
CREATE POLICY tenant_isolation_select ON lgpd_deletion_request
  FOR SELECT TO app_runtime
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON lgpd_deletion_request;
CREATE POLICY tenant_isolation_insert ON lgpd_deletion_request
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON lgpd_deletion_request;
CREATE POLICY tenant_isolation_update ON lgpd_deletion_request
  FOR UPDATE TO app_runtime
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS master_ops_all ON lgpd_deletion_request;
CREATE POLICY master_ops_all ON lgpd_deletion_request
  FOR ALL TO app_master_ops
  USING (true)
  WITH CHECK (true);

DROP TRIGGER IF EXISTS lgpd_deletion_request_master_ops_audit ON lgpd_deletion_request;
CREATE TRIGGER lgpd_deletion_request_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON lgpd_deletion_request
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
