-- 0115_inbox_seed_role_atendente.down.sql
-- SIN-63858: invert 0115 up by demoting the two seeded agent rows back
-- to 'tenant_common'. Symmetric with up — only touches the two seed
-- UUIDs and only when the current role matches the post-up value, so
-- a fresh DB or one whose roles were edited externally is not affected.
--
-- Run as app_admin. Idempotent.

BEGIN;

UPDATE users SET role = 'tenant_common'
WHERE id IN (
  '00000000-0000-0000-0000-0000000a0e01',  -- agent@acme
  '00000000-0000-0000-0000-0000000e0e02'   -- agent@globex
) AND role = 'tenant_atendente';

COMMIT;
