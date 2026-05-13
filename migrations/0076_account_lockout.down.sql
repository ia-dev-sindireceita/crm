-- 0008_account_lockout.down.sql
-- Rolls back 0008_account_lockout.up.sql. The CASCADE on the
-- foreign key from users(id) means any extant rows are dropped with
-- the table; that is the intended behaviour for a rollback (the
-- short-window counter in Redis still throttles the principal during
-- the brief window when the lockout state is gone).
BEGIN;
DROP TABLE IF EXISTS account_lockout;
COMMIT;
