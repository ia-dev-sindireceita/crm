-- 0106_tenant_custom_domains_token_issued_at.down.sql
-- SIN-63104 — Remove token_issued_at column.

BEGIN;

ALTER TABLE tenant_custom_domains
    DROP COLUMN IF EXISTS token_issued_at;

COMMIT;
