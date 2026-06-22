-- 0125_funnel_stage_dedup_and_unique.down.sql
-- SIN-65577 / SIN-65584: reverse of the up migration.
--
-- IRREVERSIBLE DATA DELETION: the up migration deletes duplicate
-- funnel_stage rows and repoints funnel_transition rows at the surviving
-- stage. That data loss is intentional (the duplicates were corruption) and
-- is NOT recreated here — there is no correct way to reconstruct the deleted
-- ids or re-split the repointed transitions. This down step only drops the
-- UNIQUE (tenant_id, key) constraint so the schema matches the pre-0125
-- shape; the de-duplicated data remains de-duplicated.
--
-- Runs as app_admin. Idempotent (IF EXISTS).

BEGIN;

ALTER TABLE funnel_stage
  DROP CONSTRAINT IF EXISTS funnel_stage_tenant_key_uniq;

COMMIT;
