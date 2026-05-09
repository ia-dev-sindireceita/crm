-- 0011_session_csrf_token.down.sql
BEGIN;
ALTER TABLE sessions DROP COLUMN IF EXISTS csrf_token;
COMMIT;
