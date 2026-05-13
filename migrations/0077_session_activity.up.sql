-- 0077_session_activity.up.sql
-- SIN-62377 (FAIL-4): give the tenant `sessions` table the columns the
-- ADR 0073 §D3 idle/hard timeout helper (internal/iam/timeouts.go) needs
-- to enforce per-role session lifetimes from the request path.
--
-- Two columns:
--
--   * last_activity — bumped on every authenticated tenant request by the
--     activity middleware (internal/adapter/httpapi/middleware/activity.go);
--     fed to iam.CheckActivity as the lastActivity argument so a session
--     that has been idle past the per-role Idle window is rejected.
--
--   * role — the iam.Role string for the principal who created this
--     session. Denormalised from users at login time (the "cheapest"
--     option from the FAIL-4 fix block) so the activity middleware can
--     pick the right Timeouts pair without a join. The value is one of
--     the four ADR 0073 §D3 strings ('master', 'tenant_gerente',
--     'tenant_atendente', 'tenant_common'); the CHECK constraint pins
--     the set so a typo at the call site shows up as an INSERT failure
--     rather than a silent fail-closed in CheckActivity.
--
-- A composite index on (tenant_id, last_activity) is added so a future
-- GC reaper can range-scan idle-expired rows without a sequential scan;
-- the existing (tenant_id, expires_at) covers the hard-cap reaper.
--
-- RLS posture: unchanged. The four-policy template (SELECT / INSERT /
-- UPDATE / DELETE) from migration 0006 already filters on tenant_id and
-- WITH CHECK (tenant_id = current_setting(...)::uuid) — the new columns
-- are not policy targets. ALTER TABLE preserves owner + policies.
--
-- Run as app_admin. Idempotent (uses IF NOT EXISTS / DROP IF EXISTS).

BEGIN;

-- last_activity. DEFAULT now() so existing rows backfill at upgrade
-- time; the upgrade window is at most one user's idle gap.
ALTER TABLE sessions
  ADD COLUMN IF NOT EXISTS last_activity timestamptz NOT NULL DEFAULT now();

-- role. DEFAULT 'tenant_common' is the broadest catch-all in
-- iam.TimeoutsForRole (Idle = 30 min, Hard = 8 h). Existing rows
-- backfill to that until the next login overwrites with the real role.
ALTER TABLE sessions
  ADD COLUMN IF NOT EXISTS role text NOT NULL DEFAULT 'tenant_common';

-- CHECK constraint. Enumerated values come from internal/iam/role.go;
-- 'master' is allowed for forward-compat even though tenant `sessions`
-- never holds a master session today (master sessions live in
-- master_session per migration 0010_master_session). The constraint is
-- DROP IF EXISTS first so re-running the migration is idempotent.
ALTER TABLE sessions
  DROP CONSTRAINT IF EXISTS sessions_role_check;
ALTER TABLE sessions
  ADD CONSTRAINT sessions_role_check
  CHECK (role IN ('master', 'tenant_gerente', 'tenant_atendente', 'tenant_common'));

-- Composite index for the future idle-row GC reaper.
CREATE INDEX IF NOT EXISTS sessions_tenant_id_last_activity_idx
  ON sessions (tenant_id, last_activity);

COMMIT;
