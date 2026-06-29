-- 0001_wa_session_roles.up.sql
-- SIN-66298 (R1.2 of SIN-66291) — dedicated least-privilege roles for the
-- WhatsApp session (whatsmeow) credential database. ADR-0108.
--
-- WHERE THIS RUNS: against the DEDICATED WhatsApp-session Postgres pointed at
-- by WA_SESSION_DATABASE_URL — NOT the app database. It lives in its own
-- migrations source (migrations/wa_session/) so the app's golang-migrate run
-- over migrations/*.sql never touches it. Apply it with, e.g.:
--
--   migrate -path migrations/wa_session \
--           -database "$WA_SESSION_SUPERUSER_DATABASE_URL" up
--
-- THIS FILE MUST BE RUN AS A SUPERUSER on the WA session cluster: CREATE ROLE
-- is a superuser-only, cluster-scoped operation (mirrors 0001_roles.up.sql for
-- the app DB). The grants below are per-database and target this WA session DB.
-- Passwords are deliberately NOT set here; ops injects them at deploy time
-- (see docs/deploy/staging.md §5g and docs/adr/0108-wa-session-credential-at-rest.md).
--
-- PRIVILEGE SPLIT (the crux — ADR-0108):
--   * wa_session_admin   — runs the whatsmeow schema DDL (sqlstore Upgrade) in
--                          a single deploy/migration step. USAGE + CREATE on the
--                          schema; owns and may ALTER/DROP the whatsmeow_* tables.
--   * wa_session_runtime — the role the APP boots as. USAGE on the schema and
--                          ONLY SELECT/INSERT/UPDATE/DELETE on whatsmeow_*. NO
--                          DDL: it cannot CREATE/ALTER/DROP. This is the DSN in
--                          WA_SESSION_DATABASE_URL.
--
-- Blast radius (the task's lens): if app_runtime is compromised it still has no
-- path to the session credential store — that store lives in a separate DB and,
-- even on a shared cluster, app_runtime/app_admin are explicitly revoked here.

BEGIN;

-- ---------------------------------------------------------------------------
-- Roles. Idempotent via DO blocks: re-running must not error (mirror 0001).
-- Neither role is a superuser, can create databases/roles, or bypasses RLS.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wa_session_admin') THEN
    CREATE ROLE wa_session_admin LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  ELSE
    ALTER ROLE wa_session_admin LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  END IF;
END $$;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wa_session_runtime') THEN
    CREATE ROLE wa_session_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  ELSE
    ALTER ROLE wa_session_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  END IF;
END $$;

-- ---------------------------------------------------------------------------
-- Schema privileges. Lock the public schema down to deny-by-default first
-- (PUBLIC keeps an implicit USAGE, and on PG<15 an implicit CREATE, otherwise),
-- then grant exactly what each role needs. runtime gets USAGE only — never
-- CREATE — so it can resolve objects but cannot run DDL.
-- ---------------------------------------------------------------------------
REVOKE ALL ON SCHEMA public FROM PUBLIC;

GRANT USAGE, CREATE ON SCHEMA public TO wa_session_admin;
GRANT USAGE ON SCHEMA public TO wa_session_runtime;

-- ---------------------------------------------------------------------------
-- Default privileges: every whatsmeow_* table the ADMIN creates later (the
-- single Upgrade-as-admin deploy step) auto-grants DML to runtime. This is
-- what lets the role migration run BEFORE the whatsmeow tables exist on a
-- first deploy — the tables inherit the grant the moment Upgrade creates them.
-- Scoped to objects created by wa_session_admin so a stray object created by
-- some other role is not silently exposed to runtime.
-- ---------------------------------------------------------------------------
ALTER DEFAULT PRIVILEGES FOR ROLE wa_session_admin IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO wa_session_runtime;

-- ---------------------------------------------------------------------------
-- Re-deploy / already-upgraded schema: grant DML on the whatsmeow_* tables
-- that already exist. Restricted to the `whatsmeow_` prefix so runtime is
-- never handed access to anything outside the session credential store.
-- ('\_' is an escaped literal underscore in LIKE, not a wildcard.)
-- ---------------------------------------------------------------------------
DO $$
DECLARE
  t record;
BEGIN
  FOR t IN
    SELECT tablename
    FROM pg_tables
    WHERE schemaname = 'public'
      AND tablename LIKE 'whatsmeow\_%'
  LOOP
    EXECUTE format(
      'GRANT SELECT, INSERT, UPDATE, DELETE ON public.%I TO wa_session_runtime',
      t.tablename
    );
  END LOOP;
END $$;

-- ---------------------------------------------------------------------------
-- Defense in depth: if the app roles happen to exist on this cluster (a
-- shared-cluster deploy rather than a separate cluster), make sure they have
-- no reach into the session credential store. A separate cluster makes this a
-- no-op; a shared cluster needs the explicit REVOKE (ADR-0108).
-- ---------------------------------------------------------------------------
DO $$
DECLARE
  r text;
BEGIN
  FOREACH r IN ARRAY ARRAY['app_runtime', 'app_admin', 'app_master_ops', 'app_audit']
  LOOP
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
      EXECUTE format('REVOKE ALL ON SCHEMA public FROM %I', r);
      EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %I', r);
      EXECUTE format('REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %I', r);
    END IF;
  END LOOP;
END $$;

COMMIT;
