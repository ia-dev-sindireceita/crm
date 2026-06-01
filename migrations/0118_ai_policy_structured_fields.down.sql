-- 0118_ai_policy_structured_fields.down.sql
-- Revert SIN-63945 / UX-F8. Drops the structured_fields column. The
-- legacy opt_in boolean carries forward as the single per-row consent
-- signal, matching the pre-F8 behaviour. A tenant whose per-field
-- selection diverged from the legacy "all-or-nothing" model loses that
-- granularity on downgrade — that is the documented rollback cost.

BEGIN;

ALTER TABLE ai_policy
  DROP COLUMN IF EXISTS structured_fields;

COMMIT;
