-- 0121_tenants_branding.up.sql
-- SIN-63963 / UX-F4: white-label branding columns on `tenants`, read by
-- the pre-auth /login page (tenancy.BrandingReader). Follows the same
-- denormalise-onto-tenants convention as 0084 / 0095 / 0108 instead of
-- introducing a tenant_settings table — the read path stays single-row
-- and outside WithTenant (the tenants table is the documented resolver
-- exception; app_runtime already holds SELECT from 0004, so no grant
-- change is needed here).
--
-- Columns:
--   * logo_url    — public URL of the tenant logo rendered on the login
--                   card. Nullable: a tenant without a logo falls back
--                   to the platform word-mark.
--   * white_label — when true the "Powered by LMHost" platform footer is
--                   suppressed. NOT NULL DEFAULT false keeps every
--                   existing tenant on the platform-attributed default,
--                   so this migration is backward-compatible.
--
-- Run as app_admin. Idempotent.

BEGIN;

ALTER TABLE tenants
  ADD COLUMN IF NOT EXISTS logo_url    text,
  ADD COLUMN IF NOT EXISTS white_label boolean NOT NULL DEFAULT false;

COMMIT;
