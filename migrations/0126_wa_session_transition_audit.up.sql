-- 0126_wa_session_transition_audit.up.sql
-- SIN-66305 (R3 / SIN-66292, origin SIN-66260 Fase 5).
--
-- The reserved, non-human SYSTEM principal that the async WhatsApp-session
-- ban/disconnect audit row is attributed to. The transition has no operator
-- in the request path, but audit_log_security.actor_user_id is FK→users(id)
-- and the SplitLogger fail-closed guard rejects a nil actor — so we seed ONE
-- reserved master row (is_master = true, tenant_id IS NULL) flagged
-- is_system = true and harden it (CTO/SecEng merge gates on SIN-66305):
--
--   * gate 1 — password_hash is the un-decodable literal 'SYSTEM-NO-LOGIN'
--     (NOT a PHC/argon2id string); password.Verify rejects it, so MasterLogin
--     always collapses to ErrInvalidCredentials. Login fails closed.
--   * gate 2 — is_system is excluded centrally at the master credential
--     reader (the single login-resolution choke point) and the master
--     directory; iam.SetPassword refuses the reserved UUID.
--   * gate 3 — fixed reserved UUID + reserved, non-deliverable email under
--     the RFC 2606 .invalid TLD.
--   * gate 4 — tenant_id IS NULL ⇒ never an impersonation target (keyed by
--     target_tenant_id).
--
-- The audit_log_security event_type vocabulary extension lives in the
-- sibling migration 0127 (it depends on the split-audit table from 0083);
-- keeping the column+seed here, with no audit dependency, lets the
-- master-ops adapter tests apply just this file to get the is_system column.
--
-- The id/email/hash below MUST match internal/iam/system_principal.go.
-- Reversibility: additive and IF-guarded; the down step deletes the
-- principal (audit rows FK ON DELETE SET NULL their actor) and drops the
-- column. Run as app_admin. Idempotent.

BEGIN;

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS is_system boolean NOT NULL DEFAULT false;

-- ON CONFLICT DO NOTHING keeps the migration idempotent under up/down/up.
-- role='master' pairs with is_master=true (satisfies users_role_chk and the
-- master⇔NULL-tenant XOR); the is_system exclusions ensure role='master' is
-- never honoured for this row.
INSERT INTO users (id, tenant_id, email, password_hash, role, is_master, is_system)
VALUES (
  '00000000-0000-0000-0000-000000005a5e',
  NULL,
  'system+wa-session@host.invalid',
  'SYSTEM-NO-LOGIN',
  'master',
  true,
  true
)
ON CONFLICT (id) DO NOTHING;

COMMIT;
