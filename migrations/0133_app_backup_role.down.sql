-- 0133_app_backup_role.down.sql
-- Reverses 0133_app_backup_role.up.sql. Run as superuser. Idempotent.
--
-- app_backup owns no objects (it is read-only: BYPASSRLS + pg_read_all_data,
-- no CREATE), so DROP ROLE cannot be blocked by object ownership. We still
-- REASSIGN/DROP OWNED defensively (mirrors 0001_roles.down.sql) so a stray
-- default-privilege grant can never wedge the rollback. A role cannot be
-- dropped while it holds a membership, so revoke pg_read_all_data first.

BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_backup') THEN
    REVOKE pg_read_all_data FROM app_backup;
    EXECUTE 'REASSIGN OWNED BY app_backup TO CURRENT_USER';
    EXECUTE 'DROP OWNED BY app_backup';
    DROP ROLE app_backup;
  END IF;
END $$;

COMMIT;
