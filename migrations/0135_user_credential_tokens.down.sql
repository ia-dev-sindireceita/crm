-- 0135_user_credential_tokens.down.sql
-- Reverse 0135: drop the invite/reset token table (policies + trigger drop
-- with it).
BEGIN;

DROP TABLE IF EXISTS user_credential_tokens;

COMMIT;
