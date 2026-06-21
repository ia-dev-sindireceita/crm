-- 0124_assignment_history_unassign.up.sql
-- SIN-65480: make the "Transferir para Não atribuído" (unassign) event
-- representable in the append-only assignment_history ledger. Deferred
-- from SIN-65473 because the ledger could not encode "assigned to nobody".
--
-- ADR (decision encoded here): the ledger records an explicit *unassign
-- event row* (reason='unassign', user_id NULL) rather than a soft-delete
-- column or a separate table. The append-only audit invariant is
-- preserved — "who leads conversation X" stays
-- `ORDER BY assigned_at DESC LIMIT 1`, and the latest row being an
-- unassign event means "assigned to nobody". Two schema changes make
-- that row legal:
--   1. user_id becomes NULLable (an unassign event names no user).
--   2. the reason CHECK gains 'unassign'.
-- A presence CHECK ties the two together so the ledger cannot drift:
-- a row is EITHER (a real user + a non-unassign reason) OR
-- (NULL user + reason='unassign').
--
-- Backward-compatible (expand phase of expand/contract): DROP NOT NULL
-- only widens the column's domain, so every existing row and all
-- pre-65480 application code — which only ever inserts a non-NULL
-- user_id with one of the three original reasons — keeps working
-- unchanged, with no backfill. The original inline reason CHECK is named
-- assignment_history_reason_check (Postgres auto-names a single-column
-- inline CHECK <table>_<column>_check); we DROP IF EXISTS it and re-add
-- the widened set under the same name. Rollback in .down.sql re-narrows
-- (see its note for the unassign-row caveat). Single transaction.

BEGIN;

ALTER TABLE assignment_history
  ALTER COLUMN user_id DROP NOT NULL;

ALTER TABLE assignment_history
  DROP CONSTRAINT IF EXISTS assignment_history_reason_check;

ALTER TABLE assignment_history
  ADD CONSTRAINT assignment_history_reason_check
    CHECK (reason IN ('lead', 'manual', 'reassign', 'unassign'));

ALTER TABLE assignment_history
  ADD CONSTRAINT assignment_history_user_presence_check
    CHECK (
      (reason = 'unassign' AND user_id IS NULL)
      OR (reason <> 'unassign' AND user_id IS NOT NULL)
    );

COMMIT;
