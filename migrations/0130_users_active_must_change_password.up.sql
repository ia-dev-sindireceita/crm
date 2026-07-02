-- 0130_users_active_must_change_password.up.sql
-- SIN-66499 (parent SIN-66492 / SIN-66493) — tenant user-management surface.
--
-- Adds two lifecycle columns to users so a tenant gerente can manage the
-- users of their own tenant from /settings/users:
--
--   * active               — deactivate-not-delete. Login checks it (a
--                            deactivated user can no longer authenticate)
--                            but the row, its history and its grants stay
--                            intact so a deactivation is reversible.
--   * must_change_password — forced-rotation marker. A user created by the
--                            gerente is seeded with a system-generated
--                            temporary password and this flag true; the
--                            forced-rotation-on-first-login enforcement is a
--                            follow-up (SecurityEngineer sign-off), so the
--                            column is added now as its backward-compatible
--                            storage seam.
--
-- Numbering: ADR 0086 fork-only numbering. The last previously-landed
-- migration on fork main is 0129_audit_log_security_channel_access, so this
-- batch starts at 0130. Mind the dup-migration landmine: verify against fork
-- main before merge.
--
-- Backward-compatible: both columns are NOT NULL with a DEFAULT, so every
-- existing row (tenant users AND is_master rows) is backfilled to
-- active=true / must_change_password=false by the DEFAULT. Existing logins
-- are unaffected (all rows land active=true). Run as app_admin. Idempotent
-- (ADD COLUMN IF NOT EXISTS).

BEGIN;

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS active boolean NOT NULL DEFAULT true;

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS must_change_password boolean NOT NULL DEFAULT false;

-- Partial index over active tenant users: the /settings/users list and the
-- last-active-gerente guardrail both scan (tenant_id, role) filtered by
-- active. Kept partial (WHERE active) so the hot path — "who are the active
-- gerentes of this tenant" — reads a compact index.
CREATE INDEX IF NOT EXISTS users_tenant_active_role_idx
  ON users (tenant_id, role)
  WHERE active AND tenant_id IS NOT NULL;

COMMIT;
