-- 0116_master_impersonation_session.down.sql
-- Inverse of 0116. Drops the table and the partial unique index in one
-- shot (DROP TABLE cascades the index). Idempotent.

BEGIN;

DROP TABLE IF EXISTS master_impersonation_session CASCADE;

COMMIT;
