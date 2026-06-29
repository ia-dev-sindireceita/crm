-- 0126_wa_session_transition_audit.down.sql
-- Reverse of 0126 up. Run as app_admin. Idempotent.
--
-- Delete the seeded principal first (audit rows that referenced it FK
-- ON DELETE SET NULL their actor_user_id), then drop the column. The
-- audit_log_security CHECK rollback lives in the sibling 0127 down step.

BEGIN;

DELETE FROM users WHERE id = '00000000-0000-0000-0000-000000005a5e' AND is_system = true;

ALTER TABLE users DROP COLUMN IF EXISTS is_system;

COMMIT;
