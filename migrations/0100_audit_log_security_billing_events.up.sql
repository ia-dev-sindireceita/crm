-- 0100_audit_log_security_billing_events.up.sql
-- SIN-62883 / Fase 2.5 C8: extend audit_log_security.event_type CHECK
-- so the master billing/wallet flows can persist their own dedicated
-- audit rows.
--
-- Three new event types join the controlled vocabulary:
--
--   * master.grant.issued        — a master operator issued a grant
--                                  (free_subscription_period or
--                                  extra_tokens). Payload mirrors the
--                                  master_grant row (grant_id, kind,
--                                  tenant_id, actor_user_id, reason,
--                                  amount?, period_days?).
--   * subscription.created       — a subscription row was inserted
--                                  (master-driven onboarding or plan
--                                  change). Payload carries
--                                  subscription_id, tenant_id, plan_id,
--                                  current_period_start, actor_user_id.
--   * invoice.cancelled_by_master — a master operator voided an invoice
--                                  with a documented reason. Payload:
--                                  invoice_id, tenant_id, period_start,
--                                  reason, actor_user_id.
--
-- audit_log_security retention (24+ months, never purged by LGPD) is
-- already in place from migration 0083 and is unchanged by this
-- migration. RLS / GRANT / index definitions on the table are also
-- preserved.
--
-- Idempotent (DROP CONSTRAINT IF EXISTS + recreate). Run as app_admin.

BEGIN;

ALTER TABLE audit_log_security
  DROP CONSTRAINT IF EXISTS audit_log_security_event_type_check;

ALTER TABLE audit_log_security
  ADD CONSTRAINT audit_log_security_event_type_check
  CHECK (event_type IN (
    'login',
    'login_fail',
    '2fa_enroll',
    '2fa_verify',
    'role_change',
    'impersonation_start',
    'impersonation_stop',
    'master_grant',
    'authz_deny',
    'authz_allow',
    'signature_fail',
    'key_rotation',
    'master.grant.issued',
    'subscription.created',
    'invoice.cancelled_by_master'
  ));

COMMIT;
