-- 0077_session_activity.down.sql
-- Roll back 0077_session_activity.up.sql. Drops the columns + index +
-- CHECK constraint. A live deployment that calls Touch / reads
-- last_activity must redeploy the previous app version BEFORE this
-- runs, otherwise app queries against last_activity / role will fail.

BEGIN;

DROP INDEX IF EXISTS sessions_tenant_id_last_activity_idx;

ALTER TABLE sessions
  DROP CONSTRAINT IF EXISTS sessions_role_check;

ALTER TABLE sessions
  DROP COLUMN IF EXISTS role;

ALTER TABLE sessions
  DROP COLUMN IF EXISTS last_activity;

COMMIT;
