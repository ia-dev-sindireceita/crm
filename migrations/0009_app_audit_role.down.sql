-- 0009_app_audit_role.down.sql
-- Reverse 0009: drop INSERT grant on audit_log, then drop the role.
-- DROP OWNED BY first so REASSIGN/REVOKE on audit_log is unnecessary
-- across databases.

BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_audit') THEN
    REVOKE ALL ON audit_log FROM app_audit;
    REVOKE ALL ON SCHEMA public FROM app_audit;
    DROP OWNED BY app_audit;
    DROP ROLE app_audit;
  END IF;
END $$;

COMMIT;
