-- 0108_tenants_dpo_settings.down.sql

BEGIN;

ALTER TABLE tenants
  DROP COLUMN IF EXISTS dpo_name,
  DROP COLUMN IF EXISTS dpo_email,
  DROP COLUMN IF EXISTS privacy_policy_version,
  DROP COLUMN IF EXISTS privacy_policy_url;

COMMIT;
