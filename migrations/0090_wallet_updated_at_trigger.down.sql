-- 0090_wallet_updated_at_trigger.down.sql
-- Reverse of the BEFORE UPDATE trigger added in 0090. The
-- set_updated_at() function is dropped because nothing else uses it
-- yet; future migrations that adopt the same pattern will need to
-- re-create it (CREATE OR REPLACE in 0090 is idempotent).

BEGIN;

DROP TRIGGER IF EXISTS token_wallet_set_updated_at ON token_wallet;
DROP FUNCTION IF EXISTS set_updated_at();

COMMIT;
