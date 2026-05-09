-- 0010_master_session.up.sql
-- Master-scope authenticated sessions (SIN-62385, ADR 0073 §D3 + ADR 0074 §4).
--
-- Run as app_admin. Idempotent.
--
-- The tenant `sessions` table from migration 0006 explicitly defers
-- master sessions to a "future PR" — this is that PR. Master sessions
-- are cross-tenant by definition (a master operator inspects, grants,
-- and impersonates across every tenant) so the four-policy RLS template
-- does NOT apply: instead we revoke from app_runtime entirely and grant
-- only to app_master_ops, then attach the master_ops_audit_trigger from
-- migration 0002 so every change lands in master_ops_audit.
--
-- mfa_verified_at is the source of truth that the RequireRecentMFA
-- middleware (PR3 of the SIN-62381 decomposition) reads to decide
-- whether a sensitive master action needs a fresh /m/2fa/verify round.
-- It is intentionally NULLable — a session that has only completed
-- password auth (not MFA) carries NULL until the verify handler runs
-- MarkVerified().

BEGIN;

CREATE TABLE IF NOT EXISTS master_session (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id           uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at        timestamptz NOT NULL DEFAULT now(),
  expires_at        timestamptz NOT NULL,
  mfa_verified_at   timestamptz,
  ip                inet,
  user_agent        text
);

-- The hot lookup is "given session id, fetch the row" served by the PK.
-- A secondary index on (user_id) lets the master console enumerate
-- live sessions for an operator (logout-everywhere, audit). A partial
-- on expires_at keeps a future GC job's range scan narrow.
CREATE INDEX IF NOT EXISTS master_session_user_id_idx
  ON master_session (user_id);

CREATE INDEX IF NOT EXISTS master_session_expires_at_idx
  ON master_session (expires_at);

ALTER TABLE master_session OWNER TO app_admin;

REVOKE ALL ON master_session FROM PUBLIC;
REVOKE ALL ON master_session FROM app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON master_session TO app_master_ops;

DROP TRIGGER IF EXISTS master_session_master_ops_audit ON master_session;
CREATE TRIGGER master_session_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON master_session
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
