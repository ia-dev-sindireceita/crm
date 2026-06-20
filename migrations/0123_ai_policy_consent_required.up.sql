-- 0123_ai_policy_consent_required.up.sql
-- SIN-65363: LGPD per-prompt consent gate becomes opt-in by config.
--
-- Adds ai_policy.consent_required BOOLEAN NOT NULL DEFAULT false.
--
-- Product decision (Pericles, board in SIN-65356): the consent modal is
-- OFF by default — the first "Resumir" click dispatches straight
-- through. This replaces the old data-hack escape hatch
-- (UPDATE ai_policy SET prompt_version='') with an explicit config flag.
--
-- Backward-compatible by construction: DEFAULT false means every
-- existing row keeps the gate OFF after this migration, matching the
-- intended new behaviour with no backfill. A tenant that wants the
-- consent flow sets consent_required=true on its scoped row.
--
-- NOTE: this is NOT a security weakening. The real PII protection — the
-- anonymizer (ai_policy.anonymize) plus the LGPD field catalogue — is
-- untouched. Only the explicit per-prompt consent modal is gated here.
--
-- Rollback: drop the column (see .down.sql). Idempotent. Single
-- transaction.

BEGIN;

ALTER TABLE ai_policy
  ADD COLUMN IF NOT EXISTS consent_required BOOLEAN NOT NULL DEFAULT false;

COMMIT;
