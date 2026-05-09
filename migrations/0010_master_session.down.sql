-- 0010_master_session.down.sql
-- Rolls back 0010_master_session.up.sql. The CASCADE on the foreign
-- key from users(id) means any extant master session rows are dropped
-- with the table; that is the intended behaviour for a rollback (any
-- live operator is forced back through /m/login on the next request).
BEGIN;
DROP TABLE IF EXISTS master_session;
COMMIT;
