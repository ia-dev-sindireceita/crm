-- 0104_tenant_palette.up.sql
-- SIN-63075 Fase 5 — White-label avançado. Persists the five-slot tenant
-- palette derived by the PaletteExtractor port (ADR-0060) from the tenant's
-- logo, or set manually via the branding admin UI (SIN-63084). The runtime
-- theming layer (SIN-63092) reads this table through internal/branding's
-- PaletteStore port.
--
-- Tenant-scoped: canonical four-policy RLS template (ADR-0072).
-- One row per tenant — PaletteWriter.SetForTenant upserts atomically; the
-- domain port has no version semantics. 30-day revert / palette history is
-- explicitly out of scope per ADR-0060 ("Manual override + 30-day revert are
-- required (lives outside this ADR)") and will be added by a producer-side
-- follow-up migration when the producer requirement lands.
--
-- Schema choices:
--
--   * tenant_id is the primary key. PaletteWriter is an upsert port, so one
--     row per tenant is the contract; the PK index also satisfies the AC's
--     "INDEX ... ON tenant_palette (tenant_id)" requirement without a
--     redundant secondary index.
--   * Colour columns are CHAR(7) holding lowercase "#rrggbb", matching
--     branding.RGB.Hex(). A regex CHECK rejects anything else so a future
--     writer that forgets to call Hex() fails at the boundary.
--   * source mirrors branding.PaletteSource.String() (extracted/fallback/
--     manual/unknown) so the adapter can persist it without a translation
--     layer. CHECK enforces the closed set.
--   * manual_overrides is jsonb defaulting to '{}' so the writer never has
--     to fabricate an empty document. Shape is producer-defined.
--   * source_logo_attachment_id is intentionally NOT a FK in this
--     migration: the tenant-logo upload table does not exist yet (ADR-0080
--     storage row will be added by a separate Fase 5 PR). The column stays
--     nullable so the constraint can be tightened in-place later via
--     ALTER TABLE ... ADD CONSTRAINT.
--
-- Run as app_admin. Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS tenant_palette (
  tenant_id                  uuid PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  primary_color              char(7) NOT NULL,
  secondary_color            char(7) NOT NULL,
  accent_color               char(7) NOT NULL,
  foreground_color           char(7) NOT NULL,
  background_color           char(7) NOT NULL,
  text_on_primary            char(7) NOT NULL,
  source                     text    NOT NULL,
  manual_overrides           jsonb   NOT NULL DEFAULT '{}'::jsonb,
  source_logo_attachment_id  uuid,
  created_at                 timestamptz NOT NULL DEFAULT now(),
  updated_at                 timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT tenant_palette_source_chk
    CHECK (source IN ('extracted', 'fallback', 'manual', 'unknown')),
  CONSTRAINT tenant_palette_primary_hex_chk
    CHECK (primary_color    ~ '^#[0-9a-f]{6}$'),
  CONSTRAINT tenant_palette_secondary_hex_chk
    CHECK (secondary_color  ~ '^#[0-9a-f]{6}$'),
  CONSTRAINT tenant_palette_accent_hex_chk
    CHECK (accent_color     ~ '^#[0-9a-f]{6}$'),
  CONSTRAINT tenant_palette_foreground_hex_chk
    CHECK (foreground_color ~ '^#[0-9a-f]{6}$'),
  CONSTRAINT tenant_palette_background_hex_chk
    CHECK (background_color ~ '^#[0-9a-f]{6}$'),
  CONSTRAINT tenant_palette_text_on_primary_hex_chk
    CHECK (text_on_primary  ~ '^#[0-9a-f]{6}$')
);

ALTER TABLE tenant_palette OWNER TO app_admin;

ALTER TABLE tenant_palette ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_palette FORCE ROW LEVEL SECURITY;

REVOKE ALL ON tenant_palette FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_palette TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_palette TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON tenant_palette;
CREATE POLICY tenant_isolation_select ON tenant_palette
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON tenant_palette;
CREATE POLICY tenant_isolation_insert ON tenant_palette
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON tenant_palette;
CREATE POLICY tenant_isolation_update ON tenant_palette
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON tenant_palette;
CREATE POLICY tenant_isolation_delete ON tenant_palette
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
