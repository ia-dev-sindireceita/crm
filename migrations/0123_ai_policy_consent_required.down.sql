-- 0123_ai_policy_consent_required.down.sql
-- Revert SIN-65363. Drops the consent_required column. After downgrade
-- the consent gate reverts to its pre-65363 trigger (deps wired +
-- prompt_version != ''), so any tenant that had set consent_required
-- loses the explicit opt-in flag and the gate is governed once again by
-- the prompt_version data-hack. That regression is the documented
-- rollback cost.

BEGIN;

ALTER TABLE ai_policy
  DROP COLUMN IF EXISTS consent_required;

COMMIT;
