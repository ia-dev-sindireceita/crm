-- 0107_lgpd_deletion_request.down.sql

BEGIN;

DROP TRIGGER IF EXISTS lgpd_deletion_request_master_ops_audit ON lgpd_deletion_request;
DROP TABLE IF EXISTS lgpd_deletion_request;

COMMIT;
