-- migrations/seed/plans.sql
-- Fase 2.5 C1 / SIN-62875: seed the three default plans in the global
-- `plan` catalogue. Idempotent: ON CONFLICT (slug) DO NOTHING so the
-- seed can be re-applied without erroring out.
--
-- Run as app_admin (BYPASSRLS=true) — the plan table is global so it has
-- no RLS, but the master_ops grant covers writes and admin owns it.
--
-- Prices are placeholders. The CEO/master operator adjusts them later
-- via the master UI (plan-doc SIN-62195 §10 — "preço dos planos em BRL"
-- is an open decision that does not block this migration).
--
-- Stable UUIDs let callers reference these plans deterministically in
-- staging fixtures and migration tests.

BEGIN;

INSERT INTO plan (id, slug, name, price_cents_brl, monthly_token_quota)
VALUES
  ('00000000-0000-0000-0000-0000000091a0',
   'free',       'Free',         0,        100000),
  ('00000000-0000-0000-0000-0000000091a1',
   'pro',        'Pro',          9900,    1000000),
  ('00000000-0000-0000-0000-0000000091a2',
   'enterprise', 'Enterprise',   49900,  10000000)
ON CONFLICT (slug) DO NOTHING;

COMMIT;
