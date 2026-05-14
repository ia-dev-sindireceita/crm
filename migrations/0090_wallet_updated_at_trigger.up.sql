-- 0090_wallet_updated_at_trigger.up.sql
-- Fase 1 / SIN-62727: BEFORE UPDATE trigger that refreshes
-- token_wallet.updated_at on every row update.
--
-- Why a separate migration from 0089:
--
--   * The CTO review on PR #78 (SIN-62725) flagged that 0089 created
--     token_wallet.updated_at with DEFAULT now() but no BEFORE UPDATE
--     trigger to refresh it. The decision (see SIN-62727 comment from
--     2026-05-14) is to land the trigger in PR5 next to the wallet
--     repository adapter so the column actually carries the "row was
--     touched" semantic the reconciliation (F37) and the operator UI
--     expect.
--
--   * set_updated_at() is a generic function: every future table that
--     wants the same semantic can reuse the trigger body. We register
--     it on token_wallet here and leave it available for follow-up
--     migrations to attach to other tables.
--
-- Run as app_admin. Idempotent (CREATE OR REPLACE FUNCTION, DROP
-- TRIGGER IF EXISTS / CREATE TRIGGER).

BEGIN;

CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS trigger AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER FUNCTION set_updated_at() OWNER TO app_admin;

DROP TRIGGER IF EXISTS token_wallet_set_updated_at ON token_wallet;
CREATE TRIGGER token_wallet_set_updated_at
  BEFORE UPDATE ON token_wallet
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMIT;
