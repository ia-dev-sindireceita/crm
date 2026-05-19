-- 0106_tenant_custom_domains_token_issued_at.up.sql
-- SIN-63104 — Add token_issued_at column to tenant_custom_domains
--
-- Persists the moment a verification_token was issued. The management
-- use-case enforces a TTL window (default 24h) on Verify, rejecting
-- stale tokens with ReasonTokenExpired. The MarkVerified compare-and-
-- swap (also part of this remediation) reads token_issued_at via the
-- same SELECT projection so audit/forensics can reconstruct rotations.
--
-- Backfill: pre-existing rows treat enrollment time as the token issue
-- time. Verified rows short-circuit Verify before the TTL check, so an
-- "expired" token on a verified row is benign. Pending rows enrolled
-- more than the TTL ago will start returning ReasonTokenExpired on the
-- next Verify — that is the correct outcome (a token outstanding past
-- the TTL should not be honored).
--
-- Idempotent: the IF NOT EXISTS guards make re-running this migration a
-- no-op. Down migration drops only the new column.

BEGIN;

ALTER TABLE tenant_custom_domains
    ADD COLUMN IF NOT EXISTS token_issued_at TIMESTAMPTZ;

UPDATE tenant_custom_domains
   SET token_issued_at = created_at
 WHERE token_issued_at IS NULL;

ALTER TABLE tenant_custom_domains
    ALTER COLUMN token_issued_at SET NOT NULL,
    ALTER COLUMN token_issued_at SET DEFAULT NOW();

COMMIT;
