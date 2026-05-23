-- 0114_users_role_check.down.sql
-- SIN-63342: revert schema-layer CHECK constraint on users.role.
--
-- Down ordering is the inverse of up:
--   1. Drop the CHECK constraint.
--   2. Revert the column DEFAULT to the historical 'agent' value
--      so the down path restores the prior schema shape exactly.
--   3. Re-mark backfilled tenant_common rows as 'agent' so a fresh
--      up re-runs the backfill. We cannot perfectly identify which
--      rows were originally 'agent' once they are 'tenant_common',
--      so the down path is best-effort by definition: it reverts
--      the schema (constraint + default) faithfully, and reseeds
--      pre-existing 'tenant_common' rows that have no totp marker
--      and are not master users back to 'agent'. New rows inserted
--      with the new default during the up window are visible only
--      by the lack of totp_required_at on tenant rows. The
--      compensating SIN-63336 seed row uses 'tenant_gerente' so it
--      is untouched here.
--
-- Run as app_admin. Idempotent.

BEGIN;

ALTER TABLE users
  DROP CONSTRAINT IF EXISTS users_role_chk;

ALTER TABLE users ALTER COLUMN role SET DEFAULT 'agent';

UPDATE users
   SET role = 'agent'
 WHERE is_master = false
   AND role = 'tenant_common';

COMMIT;
