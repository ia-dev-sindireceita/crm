-- 0133_app_backup_role.up.sql
-- Dedicated read-only backup role for pg_dump (SIN-66566, parent SIN-66536).
--
-- The scheduled backup sidecar dumps the entire schema, which carries 191 RLS
-- policies. The application role (app_runtime) is NOSUPERUSER NOBYPASSRLS, so
-- pg_dump under it fails permission-denied / silently under-dumps RLS-guarded
-- tables. `app_backup` is a login role with BYPASSRLS + pg_read_all_data: it
-- can read everything and ignore RLS, but has NO write/DDL/superuser rights.
--
-- THIS FILE MUST BE RUN AS A DATABASE SUPERUSER (CREATE ROLE is superuser-only,
-- same as 0001_roles.up.sql). The migration is idempotent via the DO block.
--
-- The password is deliberately NOT set here — ops injects it out-of-band at
-- deploy time via `\password app_backup`, mirroring app_runtime/app_admin (see
-- docs/adr/0071-postgres-roles.md "Credential injection" and the
-- "Primeira instalação" step in docs/operations/backup-restore.md). The
-- operator points the backup sidecar at this role via BACKUP_DATABASE_URL.

BEGIN;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_backup') THEN
    -- BYPASSRLS so pg_dump reads through every RLS policy; NOSUPERUSER /
    -- NOCREATEDB / NOCREATEROLE so it is read-only-with-a-key, nothing more.
    CREATE ROLE app_backup LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS;
  ELSE
    ALTER ROLE app_backup LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS;
  END IF;
END $$;

-- pg_read_all_data (predefined role, PG14+) grants SELECT on every table plus
-- USAGE on every schema/sequence — no per-table GRANT maintenance as the
-- schema grows. Combined with BYPASSRLS this is exactly "read everything for a
-- dump", with no write path.
GRANT pg_read_all_data TO app_backup;

COMMIT;
