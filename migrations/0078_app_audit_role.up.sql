-- 0078_app_audit_role.up.sql (re-landed from legacy 0009 per ADR 0086)
-- SIN-62219: dedicated app_audit role for the master-impersonation
-- audit writer.
--
-- Rationale (decisions #10/#16 of Fase 0):
--   * Non-repudiation: impersonation_started/_ended writes MUST land in
--     audit_log even if the request itself fails or RLS misconfigures.
--   * Least privilege: the writer connects as a role that can ONLY
--     INSERT into audit_log — no SELECT, no UPDATE, no DELETE, no
--     access to any other table. A compromised audit-writer credential
--     cannot read tenant data or tamper with the trail.
--   * BYPASSRLS: app_runtime's INSERT policy gates the write on
--     app.tenant_id, but the impersonation handler runs BEFORE the
--     downstream tenant scope is committed. Bypassing RLS is safe
--     because audit_log writes are otherwise constrained (one specific
--     table, INSERT only) and tenant_id is supplied explicitly by the
--     middleware, not derived from session state.
--
-- Run as app_admin. Idempotent.

BEGIN;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_audit') THEN
    CREATE ROLE app_audit LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS;
  ELSE
    ALTER ROLE app_audit LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS;
  END IF;
END $$;

GRANT USAGE ON SCHEMA public TO app_audit;

REVOKE ALL ON audit_log FROM app_audit;
GRANT INSERT ON audit_log TO app_audit;

COMMIT;
