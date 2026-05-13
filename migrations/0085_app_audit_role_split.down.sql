-- 0085_app_audit_role_split.down.sql (re-landed from legacy 0014 per ADR 0086)
-- Reverse of 0085_app_audit_role_split.up.sql.
--
-- We can't blanket REVOKE ALL because the tables themselves may already
-- be gone (if 0012 was rolled back). The conditional DO blocks make the
-- down idempotent regardless of which migrations are still applied.
--
-- Run as app_admin. Idempotent.

BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'audit_log_security') THEN
    EXECUTE 'REVOKE ALL ON audit_log_security FROM app_audit';
  END IF;
  IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'audit_log_data') THEN
    EXECUTE 'REVOKE ALL ON audit_log_data FROM app_audit';
  END IF;
END $$;

COMMIT;
