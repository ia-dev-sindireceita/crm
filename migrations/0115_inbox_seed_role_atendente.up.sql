-- 0115_inbox_seed_role_atendente.up.sql
-- SIN-63858: agent@<tenant>.<base_domain> seed users were created as
-- tenant_common in SIN-63342, but the SIN-63821 /inbox gate requires
-- tenant_atendente. Production staging is currently 403 on /inbox for
-- agent@acme.* — this migration brings the live row in line with the
-- seed file's new role string. By UUID so the UPDATE is precise even
-- if email got renamed locally.
--
-- The seed file at migrations/seed/stg.sql is the source of truth for
-- a fresh DB; this migration only matters for environments that were
-- already seeded under SIN-63342 (i.e. staging) and need an in-place
-- role flip without a full reseed.
--
-- Idempotent: the WHERE clause filters on the legacy role value, so
-- repeated `make migrate-up` runs are a no-op once the rows have been
-- promoted to tenant_atendente. The 0114 users_role_chk allowlist
-- already includes 'tenant_atendente', so the UPDATE satisfies the
-- CHECK constraint without further schema changes.
--
-- Run as app_admin.

BEGIN;

UPDATE users SET role = 'tenant_atendente'
WHERE id IN (
  '00000000-0000-0000-0000-0000000a0e01',  -- agent@acme
  '00000000-0000-0000-0000-0000000e0e02'   -- agent@globex
) AND role = 'tenant_common';

COMMIT;
