-- 0104_tenant_palette.down.sql
--
-- Reverse of the up migration. DROP TABLE cascades to the four RLS
-- policies, but they are spelled out defensively so an operator can apply
-- the down a second time (or against a partially-rolled-back DB) without
-- error. Idempotent.

BEGIN;

DROP POLICY IF EXISTS tenant_isolation_delete ON tenant_palette;
DROP POLICY IF EXISTS tenant_isolation_update ON tenant_palette;
DROP POLICY IF EXISTS tenant_isolation_insert ON tenant_palette;
DROP POLICY IF EXISTS tenant_isolation_select ON tenant_palette;

DROP TABLE IF EXISTS tenant_palette;

COMMIT;
