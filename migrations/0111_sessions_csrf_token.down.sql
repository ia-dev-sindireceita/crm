-- 0111_sessions_csrf_token.down.sql
-- Reversal of 0111_sessions_csrf_token.up.sql. Drops the column. The
-- column is fully internal to the iam adapter (no FK, no policy, no
-- index references it) so the drop is safe.
--
-- Run as app_admin. Idempotent (IF EXISTS).

BEGIN;

ALTER TABLE sessions
  DROP COLUMN IF EXISTS csrf_token;

COMMIT;
