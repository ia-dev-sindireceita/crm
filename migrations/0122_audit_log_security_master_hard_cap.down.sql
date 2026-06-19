-- 0122_audit_log_security_master_hard_cap.down.sql
-- Revert: restore the CHECK clause WITHOUT 'master.session.hard_cap_hit'.
--
-- We restore the full pre-0122 union (every other audit.SecurityEvent
-- literal, 'logout' included) rather than the narrower 0112 shape, so a
-- rollback does not accidentally drop 'logout' from the vocabulary (see
-- the union note in the .up). The only event removed on revert is the
-- one this migration added.
--
-- Will fail if any audit_log_security row already carries
-- event_type='master.session.hard_cap_hit' — that is intentional, audit
-- ledgers do not delete history. Operators MUST archive/relocate those
-- rows before reverting.

BEGIN;

ALTER TABLE audit_log_security
  DROP CONSTRAINT IF EXISTS audit_log_security_event_type_check;

ALTER TABLE audit_log_security
  ADD CONSTRAINT audit_log_security_event_type_check
  CHECK (event_type IN (
    'login',
    'login_fail',
    'logout',
    '2fa_enroll',
    '2fa_verify',
    '2fa_required',
    '2fa_recovery_used',
    '2fa_recovery_regenerated',
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
