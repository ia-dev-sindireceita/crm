-- 0118_product_category.down.sql
-- SIN-63946 / UX-F9: revert product.category addition.
--
-- Idempotent: DROP INDEX IF EXISTS then DROP COLUMN IF EXISTS. Reversing
-- this migration loses category data on existing rows; tags column on
-- product is unaffected.

BEGIN;

DROP INDEX IF EXISTS product_tenant_category_idx;

ALTER TABLE product
  DROP COLUMN IF EXISTS category;

COMMIT;
