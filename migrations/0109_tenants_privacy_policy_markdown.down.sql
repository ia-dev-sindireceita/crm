-- 0109_tenants_privacy_policy_markdown.down.sql

BEGIN;

ALTER TABLE tenants
  DROP COLUMN IF EXISTS privacy_policy_markdown,
  DROP COLUMN IF EXISTS privacy_policy_updated_at;

COMMIT;
