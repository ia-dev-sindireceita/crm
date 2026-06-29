-- 0001_wa_session_roles.down.sql
-- SIN-66298 — reverse 0001_wa_session_roles.up.sql. Run as a SUPERUSER on the
-- WA session cluster. Idempotent and safe to re-run.
--
-- Reversibility note: this revokes the DML grants and removes the dedicated
-- roles. It does NOT restore the implicit PUBLIC grants the up migration
-- revoked (that was a deliberate lock-down, not a schema change); re-grant to
-- PUBLIC manually only if you are truly rolling the database back to its
-- pre-hardening, shared-credential posture.

BEGIN;

-- Drop the default-privilege grant first so DROP ROLE does not fail on a
-- dependent default ACL. Mirror the FOR ROLE / IN SCHEMA of the up migration.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wa_session_admin')
     AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wa_session_runtime') THEN
    ALTER DEFAULT PRIVILEGES FOR ROLE wa_session_admin IN SCHEMA public
      REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM wa_session_runtime;
  END IF;
END $$;

-- Revoke every privilege the roles hold so DROP ROLE has no dependencies.
DO $$
DECLARE
  r text;
BEGIN
  FOREACH r IN ARRAY ARRAY['wa_session_runtime', 'wa_session_admin']
  LOOP
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
      EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %I', r);
      EXECUTE format('REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %I', r);
      EXECUTE format('REVOKE ALL ON SCHEMA public FROM %I', r);
    END IF;
  END LOOP;
END $$;

DROP ROLE IF EXISTS wa_session_runtime;
DROP ROLE IF EXISTS wa_session_admin;

COMMIT;
