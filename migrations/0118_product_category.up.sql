-- 0118_product_category.up.sql
-- SIN-63946 / UX-F9: add product.category for catalog categorization.
--
-- The catalog (web/catalog) renders a sidebar with category counts plus
-- a filter dropdown. The existing `tags text[]` column on product
-- (migration 0098) keeps its role as free-form labels for cases that do
-- not fit a single bucket. `category` is the explicit single-bucket
-- column the sidebar groups on.
--
-- Default '' preserves backfill semantics: existing rows land in an
-- "Sem categoria" bucket the handler renders explicitly. NOT NULL keeps
-- the read path simple — no nullable handling in the scanner or
-- template. An index on (tenant_id, category) supports the future
-- GROUP BY rollout if the in-handler aggregation outgrows in-memory
-- counting.
--
-- Run as app_admin. Idempotent.

BEGIN;

ALTER TABLE product
  ADD COLUMN IF NOT EXISTS category text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS product_tenant_category_idx
  ON product (tenant_id, category);

COMMIT;
