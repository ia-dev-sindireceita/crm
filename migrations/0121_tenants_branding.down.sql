-- 0121_tenants_branding.down.sql
-- Reverses 0121: drops the white-label branding columns. Safe because
-- the columns are additive and the /login handler degrades to the
-- platform word-mark + footer when the BrandingReader is absent.

BEGIN;

ALTER TABLE tenants
  DROP COLUMN IF EXISTS logo_url,
  DROP COLUMN IF EXISTS white_label;

COMMIT;
