-- 0124_assignment_history_unassign.down.sql
-- Revert SIN-65480. Re-narrows assignment_history to its pre-65480 shape:
-- user_id NOT NULL and reason limited to the original three values.
--
-- CAVEAT (documented rollback cost): re-adding NOT NULL fails if any
-- unassign event rows exist (user_id IS NULL). This down migration
-- therefore DELETEs the unassign rows first. That is lossy — it drops
-- the audit record that a conversation was returned to Não atribuído —
-- but it is the only way to restore the NOT NULL invariant. After
-- downgrade, affected conversations derive their leader from the last
-- surviving real assignment row (the unassign is forgotten); the
-- denormalised conversation.assigned_user_id, which is cleared on its
-- own table by a separate path, is then the only remaining signal of the
-- unassigned state. Single transaction.

BEGIN;

ALTER TABLE assignment_history
  DROP CONSTRAINT IF EXISTS assignment_history_user_presence_check;

DELETE FROM assignment_history WHERE reason = 'unassign';

ALTER TABLE assignment_history
  DROP CONSTRAINT IF EXISTS assignment_history_reason_check;

ALTER TABLE assignment_history
  ADD CONSTRAINT assignment_history_reason_check
    CHECK (reason IN ('lead', 'manual', 'reassign'));

ALTER TABLE assignment_history
  ALTER COLUMN user_id SET NOT NULL;

COMMIT;
