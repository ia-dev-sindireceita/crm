-- 0108_tenants_dpo_settings.up.sql
-- SIN-63186 / Fase 6 PR3 (AC #5): DPO contact + privacy-policy
-- versioning columns on `tenants`. The existing convention is to
-- denormalise tenant-wide settings onto the `tenants` row (see
-- 0084_tenant_audit_data_retention and 0095_tenants_default_lead_user_id);
-- this migration follows the same pattern instead of introducing a
-- new tenant_settings table — keeping the read-path single-row.
--
-- Columns:
--   * dpo_name             — display name of the data protection officer.
--   * dpo_email            — contact e-mail; format validated at the
--                            application layer (CHECK only enforces
--                            non-empty when set).
--   * privacy_policy_version — semver / date tag of the published
--                              policy the tenant is currently bound to.
--   * privacy_policy_url   — public URL of the currently published policy.
--
-- All four columns are nullable so existing tenants stay valid until
-- the master operator fills them in.
--
-- Run as app_admin. Idempotent.

BEGIN;

ALTER TABLE tenants
  ADD COLUMN IF NOT EXISTS dpo_name              text,
  ADD COLUMN IF NOT EXISTS dpo_email             text,
  ADD COLUMN IF NOT EXISTS privacy_policy_version text,
  ADD COLUMN IF NOT EXISTS privacy_policy_url    text;

COMMIT;
