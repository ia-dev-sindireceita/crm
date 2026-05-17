-- migrations/seed/token_packages.sql
-- Fase 4 / SIN-62952: seed the D4-ratified token packages.
--
-- Same shape rationale as seed/plans.sql: idempotent (ON CONFLICT (slug)
-- DO NOTHING) so re-applying the seed across deploys is safe. Stable
-- UUIDs let staging fixtures and migration tests reference these
-- packages deterministically.
--
-- Prices (Small R$ 15 / Medium R$ 49 / Large R$ 149) and bundle sizes
-- (1M / 5M / 20M tokens) come from the D4 ratification on 2026-05-09
-- ([SIN-62207](issue tracker), interactions `012928fd` + `b655b91e`).
-- The markup math + revision premises are documented in the
-- 0101_phase4_marketing_billing_dunning.up.sql comment block.
--
-- Run as app_admin (BYPASSRLS=true). token_packages has no RLS (catálogo),
-- but app_master_ops owns writes — so the operator MUST run this as
-- admin or master_ops.

BEGIN;

INSERT INTO token_packages (id, slug, kind, name, tokens, price_cents_brl)
VALUES
  ('00000000-0000-0000-0000-0000000094a0',
   'small',   'tokens', 'Small',   1000000,   1500),
  ('00000000-0000-0000-0000-0000000094a1',
   'medium',  'tokens', 'Medium',  5000000,   4900),
  ('00000000-0000-0000-0000-0000000094a2',
   'large',   'tokens', 'Large',  20000000,  14900)
ON CONFLICT (slug) DO NOTHING;

COMMIT;
