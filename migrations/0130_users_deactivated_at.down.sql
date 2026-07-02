-- 0130_users_deactivated_at.down.sql
-- Reverse 0130: drop the soft-deactivation column + its partial index.
BEGIN;

DROP INDEX IF EXISTS users_tenant_active_idx;
ALTER TABLE users DROP COLUMN IF EXISTS deactivated_at;

COMMIT;
