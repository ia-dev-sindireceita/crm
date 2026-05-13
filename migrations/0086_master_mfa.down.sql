-- 0086_master_mfa.down.sql
-- Rolls back 0086_master_mfa.up.sql. Drops the recovery code table
-- before master_mfa so no FK ordering issue arises if a future
-- migration adds one. The triggers go away with the tables.
BEGIN;
DROP TABLE IF EXISTS master_recovery_code;
DROP TABLE IF EXISTS master_mfa;
COMMIT;
