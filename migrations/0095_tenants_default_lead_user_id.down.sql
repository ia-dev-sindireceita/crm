-- 0095_tenants_default_lead_user_id.down.sql
-- Reverts the column + sparse index introduced by the .up. The FK is
-- dropped implicitly when the column is dropped.

BEGIN;

DROP INDEX IF EXISTS tenants_default_lead_user_idx;

ALTER TABLE tenants
  DROP COLUMN IF EXISTS default_lead_user_id;

COMMIT;
