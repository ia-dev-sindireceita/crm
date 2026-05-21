-- 0111_sessions_csrf_token.up.sql
-- SIN-63222: persist the per-session CSRF token alongside the existing
-- sessions row. The token is minted at login (internal/iam/login.go via
-- iam/csrf.GenerateToken) and mirrored into the __Host-csrf cookie; the
-- RequireCSRF middleware re-hydrates iam.Session via SessionStore.Get on
-- every state-changing request and compares the header value against
-- session.CSRFToken under constant time (ADR 0073 §D1 step 6).
--
-- Before this migration the column did not exist, so SessionStore.Get
-- returned CSRFToken="" on every request after login and csrf.Verify
-- collapsed to ErrSessionTokenMissing — POST /logout (and every other
-- POST/PATCH/DELETE on an authenticated route) returned 403 with reason
-- "csrf.token_missing" even when cookies and the X-CSRF-Token header
-- matched. The defect is end-to-end-confirmed in pen-test SIN-63190 and
-- triaged in SIN-63217.
--
-- Default '' is intentional: sessions that pre-date this migration keep
-- a row but their CSRFToken is empty, so they continue to fail CSRF until
-- the user re-logs in. Acceptable because nobody can perform a
-- state-changing action on the affected sessions today — a single
-- re-login per affected user replaces the row with a fresh token. No
-- random backfill: those rows already need a new cookie pair, and minting
-- a token without mirroring it into the user's browser cookie buys nothing.
--
-- RLS posture: unchanged. The four-policy template (SELECT / INSERT /
-- UPDATE / DELETE) from migration 0006_create_sessions filters on
-- tenant_id and WITH CHECK (tenant_id = current_setting(...)::uuid). The
-- new column is not a policy target. ALTER TABLE preserves owner and
-- policies.
--
-- Numbering note: 0111 was the next free index after Fase 6 siblings
-- landed on main. The original 0107 collision between user_mfa and
-- consent_record was resolved by SIN-63230 by renumbering them to
-- 0112_user_mfa and 0113_consent_record; slot 0107 remains
-- 0107_lgpd_deletion_request (+ 0108_tenants_dpo_settings,
-- 0109_tenants_privacy_policy_markdown, 0110_audit_log_security_logout).
-- Run as app_admin.
-- Idempotent (IF NOT EXISTS).

BEGIN;

ALTER TABLE sessions
  ADD COLUMN IF NOT EXISTS csrf_token text NOT NULL DEFAULT '';

COMMIT;
