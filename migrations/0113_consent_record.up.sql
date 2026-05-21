-- 0113_consent_record.up.sql
-- SIN-63185 / Fase 6 PR2: generic LGPD consent ledger for non-AI
-- (Renumbered from 0107 by SIN-63230 to resolve a three-way 0107 collision.)
-- purposes (terms_of_service, privacy_policy, marketing,
-- cookies_analytics). The AI-specific ai_policy_consent table
-- (migration 0101) stays unchanged — see ADR 0101 for the rationale.
--
-- Schema invariants:
--   * One row per (tenant_id, subject_type, subject_id, purpose,
--     version). The UNIQUE constraint enforces idempotence on
--     Record(subject, purpose, version): a repeat call with the
--     same triple collapses to ON CONFLICT DO NOTHING in the
--     adapter, not a duplicate row.
--   * Revocation is in-place on the existing row: granted flips to
--     false, revoked_at and revoke_reason populate. History remains
--     intact (one row per version, with both grant and revoke
--     timestamps).
--   * subject_type is restricted to ('user','contact','tenant') —
--     adding a new subject category requires a follow-up migration.
--   * purpose is restricted to the four non-AI LGPD purposes above —
--     same wire-stability rule.
--
-- RLS posture mirrors ai_policy_consent (0101): FORCE RLS, tenant-
-- isolated SELECT/INSERT/UPDATE/DELETE for app_runtime, full access
-- for app_master_ops, master_ops_audit_trigger for the cross-tenant
-- write trail.
--
-- audit_log_data event_type CHECK clause is extended to accept
-- 'consent_grant' and 'consent_revoke' so the ConsentRegistry
-- decorator can emit one DataEvent per Record/Revoke (with IP and
-- user-agent in target). Down step rolls the CHECK back to its
-- 0083_split_audit_log shape; this is safe only when no
-- consent_record audit rows exist (developer rollback path, not a
-- production reverse).
--
-- Run as app_admin. Idempotent: CREATE TABLE IF NOT EXISTS, DROP
-- POLICY IF EXISTS, DROP TRIGGER IF EXISTS, DROP CONSTRAINT IF
-- EXISTS on the audit_log_data CHECK.

BEGIN;

CREATE TABLE IF NOT EXISTS consent_record (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  subject_type    text NOT NULL
                    CHECK (subject_type IN ('user','contact','tenant')),
  subject_id      text NOT NULL,
  purpose         text NOT NULL
                    CHECK (purpose IN (
                      'terms_of_service',
                      'privacy_policy',
                      'marketing',
                      'cookies_analytics'
                    )),
  version         text NOT NULL,
  granted         boolean NOT NULL DEFAULT true,
  granted_at      timestamptz NOT NULL DEFAULT now(),
  revoked_at      timestamptz,
  revoke_reason   text,
  ip              inet,
  user_agent      text,
  CONSTRAINT consent_record_subject_version_uniq
    UNIQUE (tenant_id, subject_type, subject_id, purpose, version),
  CONSTRAINT consent_record_revoke_consistency
    CHECK (
      (granted = true  AND revoked_at IS NULL  AND revoke_reason IS NULL) OR
      (granted = false AND revoked_at IS NOT NULL)
    )
);

ALTER TABLE consent_record OWNER TO app_admin;

-- Tenant-scoped reads: "all consents for this tenant" probe; the
-- UNIQUE constraint already covers the per-row identity probe.
CREATE INDEX IF NOT EXISTS consent_record_tenant_idx
  ON consent_record (tenant_id);

-- "Latest / history for (subject, purpose)" probe: the resolver
-- queries by (tenant_id, subject_type, subject_id, purpose) ordered
-- by granted_at DESC; the multi-column index satisfies both Latest
-- (LIMIT 1) and History (full slice).
CREATE INDEX IF NOT EXISTS consent_record_subject_purpose_idx
  ON consent_record (tenant_id, subject_type, subject_id, purpose, granted_at DESC);

ALTER TABLE consent_record ENABLE ROW LEVEL SECURITY;
ALTER TABLE consent_record FORCE ROW LEVEL SECURITY;

REVOKE ALL ON consent_record FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON consent_record TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON consent_record TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON consent_record;
CREATE POLICY tenant_isolation_select ON consent_record
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON consent_record;
CREATE POLICY tenant_isolation_insert ON consent_record
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON consent_record;
CREATE POLICY tenant_isolation_update ON consent_record
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON consent_record;
CREATE POLICY tenant_isolation_delete ON consent_record
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS master_ops_select ON consent_record;
CREATE POLICY master_ops_select ON consent_record
  FOR SELECT TO app_master_ops
  USING (true);

DROP POLICY IF EXISTS master_ops_insert ON consent_record;
CREATE POLICY master_ops_insert ON consent_record
  FOR INSERT TO app_master_ops
  WITH CHECK (true);

DROP POLICY IF EXISTS master_ops_update ON consent_record;
CREATE POLICY master_ops_update ON consent_record
  FOR UPDATE TO app_master_ops
  USING (true)
  WITH CHECK (true);

DROP POLICY IF EXISTS master_ops_delete ON consent_record;
CREATE POLICY master_ops_delete ON consent_record
  FOR DELETE TO app_master_ops
  USING (true);

DROP TRIGGER IF EXISTS consent_record_master_ops_audit ON consent_record;
CREATE TRIGGER consent_record_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON consent_record
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- audit_log_data event_type vocabulary: add 'consent_grant' and
-- 'consent_revoke' so the ConsentRegistry decorator can record one
-- DataEvent per Record/Revoke (with IP/UA inside target). The CHECK
-- clause is replaced (not amended) because PostgreSQL named CHECK
-- constraints are immutable — DROP + ADD is the only path.
-- ---------------------------------------------------------------------------

ALTER TABLE audit_log_data DROP CONSTRAINT IF EXISTS audit_log_data_event_type_check;
ALTER TABLE audit_log_data
  ADD CONSTRAINT audit_log_data_event_type_check
  CHECK (event_type IN (
    'read_pii',
    'write_contact',
    'export_csv',
    'lgpd_export',
    'lgpd_forget',
    'consent_grant',
    'consent_revoke'
  ));

COMMIT;
