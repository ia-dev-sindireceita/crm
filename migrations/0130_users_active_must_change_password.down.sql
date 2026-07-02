-- 0130_users_active_must_change_password.down.sql
-- Reverse of 0130 up (SIN-66499). Drops the partial index first, then the
-- two lifecycle columns. IF EXISTS keeps the rollback safe to run twice.
--
-- Reversibility note: dropping `active` means every user becomes loginnable
-- again (the LookupCredentials AND-active predicate is compiled out on the
-- app side by a matching code rollback). Only run on a developer rollback
-- where no deactivated user must stay locked out. Run as app_admin.

BEGIN;

DROP INDEX IF EXISTS users_tenant_active_role_idx;

ALTER TABLE users DROP COLUMN IF EXISTS must_change_password;
ALTER TABLE users DROP COLUMN IF EXISTS active;

COMMIT;
