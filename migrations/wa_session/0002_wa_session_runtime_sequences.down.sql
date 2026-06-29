-- 0002_wa_session_runtime_sequences.down.sql
-- SIN-66311 — reverse 0002_wa_session_runtime_sequences.up.sql. Run as a
-- SUPERUSER on the WA session cluster. Idempotent and safe to re-run.
--
-- golang-migrate runs down migrations in reverse order, so this executes
-- BEFORE 0001's down (which drops the roles). Revoking the sequence default
-- privilege here keeps 0001's DROP ROLE free of dependent default ACLs.

BEGIN;

-- Drop the default-privilege grant first (mirror the FOR ROLE / IN SCHEMA of
-- the up migration). Guarded so it does not error if a role is already gone.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wa_session_admin')
     AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wa_session_runtime') THEN
    ALTER DEFAULT PRIVILEGES FOR ROLE wa_session_admin IN SCHEMA public
      REVOKE USAGE, SELECT ON SEQUENCES FROM wa_session_runtime;
  END IF;
END $$;

-- Revoke the explicit grants the loop may have handed out on existing
-- whatsmeow_* sequences.
DO $$
DECLARE
  s record;
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wa_session_runtime') THEN
    FOR s IN
      SELECT sequencename
      FROM pg_sequences
      WHERE schemaname = 'public'
        AND sequencename LIKE 'whatsmeow\_%'
    LOOP
      EXECUTE format(
        'REVOKE USAGE, SELECT ON SEQUENCE public.%I FROM wa_session_runtime',
        s.sequencename
      );
    END LOOP;
  END IF;
END $$;

COMMIT;
