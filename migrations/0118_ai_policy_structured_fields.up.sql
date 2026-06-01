-- 0118_ai_policy_structured_fields.up.sql
-- SIN-63945 / UX-F8: per-field LGPD opt-in for the AI policy editor.
--
-- Adds ai_policy.structured_fields TEXT[] NOT NULL DEFAULT '{}'.
-- Backfills the column from the legacy opt_in boolean so an existing
-- tenant's effective field set does NOT change at deploy time:
--
--   * opt_in = true  →  structured_fields = {email, phone, cnpj}
--                       (the closed Yellow allow-list per lgpd-field-spec)
--   * opt_in = false →  structured_fields = '{}' (the column default)
--
-- opt_in is intentionally kept for now: the resolver and the wallet
-- gate still read it. A later migration will deprecate it once every
-- consumer reads structured_fields directly.
--
-- The allow-list (Green/Yellow) and the LGPD-blocked Red list are
-- enforced at the policy gate (internal/aipolicy.ValidateStructuredFields)
-- — not at the column. The column accepts any string array so a future
-- ADR can extend the allow-list without a column-level migration.
--
-- Idempotent. Single transaction.

BEGIN;

ALTER TABLE ai_policy
  ADD COLUMN IF NOT EXISTS structured_fields TEXT[] NOT NULL DEFAULT '{}'::text[];

-- Backfill from the legacy boolean. The Yellow set is the SE-authored
-- allow-list (email, phone, cnpj) — see lgpd-field-spec §"Tier classification".
UPDATE ai_policy
   SET structured_fields = ARRAY['email','phone','cnpj']::text[]
 WHERE opt_in = true
   AND structured_fields = '{}'::text[];

COMMIT;
