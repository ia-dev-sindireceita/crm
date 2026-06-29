-- 0002_wa_session_runtime_sequences.up.sql
-- SIN-66311 (follow-up to SIN-66298 / ADR-0108) — extend the runtime grant to
-- cover SEQUENCES on a future whatsmeow schema bump.
--
-- WHERE THIS RUNS: same as 0001 — against the DEDICATED WhatsApp-session
-- Postgres pointed at by WA_SESSION_DATABASE_URL, applied with a SUPERUSER DSN
-- on the WA session cluster (see migrations/wa_session/README.md and
-- docs/deploy/staging.md §5g).
--
-- WHY: 0001 grants wa_session_runtime DML on TABLES only (ALTER DEFAULT
-- PRIVILEGES … ON TABLES + the whatsmeow_% grant loop). Today's whatsmeow
-- schema uses natural/composite keys — no SERIAL/IDENTITY columns — so there
-- are no sequences and the runtime never needs sequence privileges. That is a
-- fail-CLOSED posture (availability, not confidentiality): correct today.
--
-- The latent risk: a future whatsmeow library bump that introduces a SERIAL or
-- IDENTITY column creates an owned sequence. wa_session_runtime's INSERT would
-- then need USAGE on that sequence to call nextval(); without it the first
-- INSERT after the schema bump fails 42501 (insufficient_privilege) on the
-- sequence. This migration makes the grant secure-by-default so that bump is a
-- non-event for the runtime role — while keeping the privilege minimal:
--   * USAGE  → nextval()/currval() (what a SERIAL/IDENTITY INSERT needs).
--   * SELECT → currval()/last_value reads.
--   * NOT UPDATE (no setval) and NOT any DDL — runtime still cannot CREATE,
--     ALTER, or DROP a sequence. Sequence DDL stays an admin-only deploy step.
--
-- Mirrors the TABLES grant in 0001 exactly: default privileges are scoped to
-- objects created by wa_session_admin (the role that runs the whatsmeow
-- Upgrade), and the grant loop is restricted to the whatsmeow_ prefix, so a
-- stray sequence created by some other role is never silently exposed.

BEGIN;

-- ---------------------------------------------------------------------------
-- First-deploy / future-bump path: every sequence the ADMIN creates later
-- (the Upgrade-as-admin deploy step, when a bumped schema introduces one)
-- auto-grants USAGE+SELECT to runtime. Scoped to wa_session_admin's own
-- objects, mirroring the ON TABLES default privilege in 0001.
-- ---------------------------------------------------------------------------
ALTER DEFAULT PRIVILEGES FOR ROLE wa_session_admin IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO wa_session_runtime;

-- ---------------------------------------------------------------------------
-- Re-deploy / already-upgraded schema: grant USAGE+SELECT on the whatsmeow_*
-- sequences that already exist (none on today's schema — this is a no-op until
-- a bump introduces one). Restricted to the `whatsmeow_` prefix so runtime is
-- never handed access to anything outside the session credential store.
-- ('\_' is an escaped literal underscore in LIKE, not a wildcard.)
-- ---------------------------------------------------------------------------
DO $$
DECLARE
  s record;
BEGIN
  FOR s IN
    SELECT sequencename
    FROM pg_sequences
    WHERE schemaname = 'public'
      AND sequencename LIKE 'whatsmeow\_%'
  LOOP
    EXECUTE format(
      'GRANT USAGE, SELECT ON SEQUENCE public.%I TO wa_session_runtime',
      s.sequencename
    );
  END LOOP;
END $$;

COMMIT;
