-- 0125_funnel_stage_dedup_and_unique.up.sql
-- SIN-65577 / SIN-65584: de-dup funnel_stage rows and (re)enforce the
-- UNIQUE (tenant_id, key) invariant at the schema boundary.
--
-- Background
-- ----------
-- 0093 declares funnel_stage with CONSTRAINT funnel_stage_tenant_key_uniq
-- UNIQUE (tenant_id, key) via CREATE TABLE IF NOT EXISTS. On any database
-- whose funnel_stage table was created by an earlier 0093 variant that
-- lacked the constraint, the IF NOT EXISTS made the constraint a no-op, so
-- it was never added. With the invariant missing, the idempotent seed's
-- ON CONFLICT (tenant_id, key) DO NOTHING had nothing to conflict on and a
-- second seed inserted a full duplicate set (10 stages/tenant, two rows per
-- key with distinct ids). The Board adapter dedups by stage id, so both
-- rows of a key surfaced as separate columns (QA defect SIN-65577).
--
-- This migration is tenant-agnostic and idempotent: it fixes every affected
-- tenant and is a clean no-op on a database that already has the constraint
-- and no duplicates.
--
-- Order matters. funnel_transition.from_stage_id / to_stage_id reference
-- funnel_stage(id) ON DELETE RESTRICT, so every transition pointing at a
-- loser row must be repointed at the survivor BEFORE the losers are
-- deleted, otherwise the DELETE would be blocked by the FK.
--
-- Survivor selection is deterministic: per (tenant_id, key) group, keep the
-- row with the lowest position, breaking ties by lowest id.
--
-- Runs as app_admin. The master_ops_audit_trigger is a no-op for app_admin
-- (it only audits app_master_ops), so the UPDATE/DELETE below do not require
-- the app.master_ops_actor_user_id GUC.

BEGIN;

-- ---------------------------------------------------------------------------
-- Step 1a: repoint funnel_transition.from_stage_id (nullable) off losers.
-- ---------------------------------------------------------------------------
WITH ranked AS (
  SELECT id,
         first_value(id) OVER (
           PARTITION BY tenant_id, key
           ORDER BY position ASC, id ASC
         ) AS survivor_id
    FROM funnel_stage
),
losers AS (
  SELECT id AS loser_id, survivor_id
    FROM ranked
   WHERE id <> survivor_id
)
UPDATE funnel_transition ft
   SET from_stage_id = l.survivor_id
  FROM losers l
 WHERE ft.from_stage_id = l.loser_id;

-- ---------------------------------------------------------------------------
-- Step 1b: repoint funnel_transition.to_stage_id (NOT NULL) off losers.
-- ---------------------------------------------------------------------------
WITH ranked AS (
  SELECT id,
         first_value(id) OVER (
           PARTITION BY tenant_id, key
           ORDER BY position ASC, id ASC
         ) AS survivor_id
    FROM funnel_stage
),
losers AS (
  SELECT id AS loser_id, survivor_id
    FROM ranked
   WHERE id <> survivor_id
)
UPDATE funnel_transition ft
   SET to_stage_id = l.survivor_id
  FROM losers l
 WHERE ft.to_stage_id = l.loser_id;

-- ---------------------------------------------------------------------------
-- Step 2: delete the loser rows. After step 1 no transition references a
-- loser, so the ON DELETE RESTRICT FK no longer blocks the delete.
-- ---------------------------------------------------------------------------
WITH ranked AS (
  SELECT id,
         first_value(id) OVER (
           PARTITION BY tenant_id, key
           ORDER BY position ASC, id ASC
         ) AS survivor_id
    FROM funnel_stage
),
losers AS (
  SELECT id AS loser_id
    FROM ranked
   WHERE id <> survivor_id
)
DELETE FROM funnel_stage s
 USING losers l
 WHERE s.id = l.loser_id;

-- ---------------------------------------------------------------------------
-- Step 3: (re)add the UNIQUE (tenant_id, key) constraint idempotently. The
-- data is clean after steps 1-2, so the ADD cannot fail. Guarded on
-- pg_constraint so re-running (or running where 0093 already added it) is a
-- no-op.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
      FROM pg_constraint
     WHERE conname = 'funnel_stage_tenant_key_uniq'
       AND conrelid = 'funnel_stage'::regclass
  ) THEN
    ALTER TABLE funnel_stage
      ADD CONSTRAINT funnel_stage_tenant_key_uniq UNIQUE (tenant_id, key);
  END IF;
END $$;

COMMIT;
