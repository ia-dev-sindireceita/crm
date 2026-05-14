-- 0088_inbox_contacts.up.sql
-- Fase 1 inbox/contacts/dedup tables (SIN-62724, child of SIN-62193).
--
-- Numbering: ADR 0086 fork-only numbering. 0087_master_session is the last
-- previously-landed fork-only migration, so this batch starts at 0088. The
-- issue body suggested 0087_*; that slot was taken by the master-session
-- batch (SIN-62525) before this PR landed.
--
-- Tables (all six in one migration because they form a single deployable
-- feature unit — same pattern as 0083_split_audit_log which creates
-- audit_log_security + audit_log_data + a unified view in one file):
--
--   * contact                    — per-tenant person/account record
--   * contact_channel_identity   — (channel, external_id) → contact mapping
--   * conversation               — per-tenant open/closed conversation thread
--   * message                    — per-conversation inbound/outbound message
--   * assignment                 — per-conversation user assignment history
--   * inbound_message_dedup      — global canonical idempotency ledger
--
-- Run as app_admin. Idempotent.
--
-- All tenant-scoped tables follow the canonical four-policy RLS template from
-- docs/adr/0072-rls-policies.md. tenant_id is denormalized onto every child
-- table so the policy USING/WITH CHECK clauses are index-backed (the ADR
-- 0072 §process rule #6).
--
-- inbound_message_dedup is intentionally NOT tenant-scoped. It is consulted
-- by the webhook receiver before tenant context has been fully resolved
-- (ADR 0087, see SIN-62723): the same (channel, channel_external_id) pair
-- never reaches Postgres twice regardless of which tenant the body claims.

BEGIN;

-- ---------------------------------------------------------------------------
-- contact
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS contact (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  display_name  text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS contact_tenant_id_idx
  ON contact (tenant_id);

ALTER TABLE contact OWNER TO app_admin;
ALTER TABLE contact ENABLE ROW LEVEL SECURITY;
ALTER TABLE contact FORCE ROW LEVEL SECURITY;

REVOKE ALL ON contact FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON contact TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON contact TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON contact;
CREATE POLICY tenant_isolation_select ON contact
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON contact;
CREATE POLICY tenant_isolation_insert ON contact
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON contact;
CREATE POLICY tenant_isolation_update ON contact
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON contact;
CREATE POLICY tenant_isolation_delete ON contact
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS contact_master_ops_audit ON contact;
CREATE TRIGGER contact_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON contact
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- contact_channel_identity
-- Maps (channel, external_id) → contact. The UNIQUE(channel, external_id)
-- spans tenants on purpose: an inbound webhook only knows the sender's
-- phone number (E.164), and the receiver resolves that to exactly one
-- contact globally. tenant_id is denormalized for RLS performance.
-- UNIQUE(contact_id, channel) ensures a single contact has at most one
-- identity per channel (a contact cannot have two WhatsApp numbers
-- attached at the same time — split them into two contacts if needed).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS contact_channel_identity (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  contact_id   uuid NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
  channel      text NOT NULL,
  external_id  text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT contact_channel_identity_channel_external_uniq
    UNIQUE (channel, external_id),
  CONSTRAINT contact_channel_identity_contact_channel_uniq
    UNIQUE (contact_id, channel)
);

CREATE INDEX IF NOT EXISTS contact_channel_identity_tenant_idx
  ON contact_channel_identity (tenant_id);

ALTER TABLE contact_channel_identity OWNER TO app_admin;
ALTER TABLE contact_channel_identity ENABLE ROW LEVEL SECURITY;
ALTER TABLE contact_channel_identity FORCE ROW LEVEL SECURITY;

REVOKE ALL ON contact_channel_identity FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON contact_channel_identity TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON contact_channel_identity TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON contact_channel_identity;
CREATE POLICY tenant_isolation_select ON contact_channel_identity
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON contact_channel_identity;
CREATE POLICY tenant_isolation_insert ON contact_channel_identity
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON contact_channel_identity;
CREATE POLICY tenant_isolation_update ON contact_channel_identity
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON contact_channel_identity;
CREATE POLICY tenant_isolation_delete ON contact_channel_identity
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS contact_channel_identity_master_ops_audit ON contact_channel_identity;
CREATE TRIGGER contact_channel_identity_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON contact_channel_identity
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- conversation
-- assigned_user_id is nullable + ON DELETE SET NULL so a user deletion does
-- not lose conversation history. The hot inbox query is "per tenant, list
-- open conversations newest-message-first", covered by
-- (tenant_id, state, last_message_at DESC).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS conversation (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  contact_id        uuid NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
  channel           text NOT NULL,
  state             text NOT NULL DEFAULT 'open'
                      CHECK (state IN ('open', 'closed')),
  assigned_user_id  uuid REFERENCES users(id) ON DELETE SET NULL,
  last_message_at   timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS conversation_tenant_state_last_msg_idx
  ON conversation (tenant_id, state, last_message_at DESC);
CREATE INDEX IF NOT EXISTS conversation_contact_idx
  ON conversation (contact_id);

ALTER TABLE conversation OWNER TO app_admin;
ALTER TABLE conversation ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversation FORCE ROW LEVEL SECURITY;

REVOKE ALL ON conversation FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON conversation TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON conversation TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON conversation;
CREATE POLICY tenant_isolation_select ON conversation
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON conversation;
CREATE POLICY tenant_isolation_insert ON conversation
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON conversation;
CREATE POLICY tenant_isolation_update ON conversation
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON conversation;
CREATE POLICY tenant_isolation_delete ON conversation
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS conversation_master_ops_audit ON conversation;
CREATE TRIGGER conversation_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON conversation
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- message
-- direction: 'in' (received from contact) | 'out' (sent to contact).
-- status:    'pending' → 'sent' → 'delivered' → 'read', or 'failed'.
-- channel_external_id is the Meta wamid (or equivalent vendor message id).
-- It is nullable on outbound until the vendor acks the send, and on
-- inbound it carries the vendor id for reconciliation against the dedup
-- ledger (which is keyed on the SAME pair below).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS message (
  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id             uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  conversation_id       uuid NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
  direction             text NOT NULL CHECK (direction IN ('in', 'out')),
  body                  text NOT NULL DEFAULT '',
  status                text NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending', 'sent', 'delivered', 'read', 'failed')),
  channel_external_id   text,
  sent_by_user_id       uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS message_conversation_created_idx
  ON message (conversation_id, created_at DESC);
CREATE INDEX IF NOT EXISTS message_tenant_idx
  ON message (tenant_id);

ALTER TABLE message OWNER TO app_admin;
ALTER TABLE message ENABLE ROW LEVEL SECURITY;
ALTER TABLE message FORCE ROW LEVEL SECURITY;

REVOKE ALL ON message FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON message TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON message TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON message;
CREATE POLICY tenant_isolation_select ON message
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON message;
CREATE POLICY tenant_isolation_insert ON message
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON message;
CREATE POLICY tenant_isolation_update ON message
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON message;
CREATE POLICY tenant_isolation_delete ON message
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS message_master_ops_audit ON message;
CREATE TRIGGER message_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON message
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- assignment
-- Append-only-ish: an assignment row is created when a user is assigned and
-- unassigned_at is filled when the assignment ends. user_id is NOT NULL +
-- ON DELETE CASCADE so a removed user does not leave dangling rows; the
-- audit trail of who was assigned lives in master_ops_audit via the
-- trigger below.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS assignment (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  conversation_id  uuid NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
  user_id          uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  assigned_at      timestamptz NOT NULL DEFAULT now(),
  unassigned_at    timestamptz
);

CREATE INDEX IF NOT EXISTS assignment_conversation_idx
  ON assignment (conversation_id, assigned_at DESC);
CREATE INDEX IF NOT EXISTS assignment_tenant_idx
  ON assignment (tenant_id);

ALTER TABLE assignment OWNER TO app_admin;
ALTER TABLE assignment ENABLE ROW LEVEL SECURITY;
ALTER TABLE assignment FORCE ROW LEVEL SECURITY;

REVOKE ALL ON assignment FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON assignment TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON assignment TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON assignment;
CREATE POLICY tenant_isolation_select ON assignment
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON assignment;
CREATE POLICY tenant_isolation_insert ON assignment
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON assignment;
CREATE POLICY tenant_isolation_update ON assignment
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON assignment;
CREATE POLICY tenant_isolation_delete ON assignment
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS assignment_master_ops_audit ON assignment;
CREATE TRIGGER assignment_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON assignment
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- inbound_message_dedup
-- Global canonical idempotency ledger (ADR 0087 / SIN-62723). NOT tenant
-- scoped: the webhook receiver consults it before tenant context is fully
-- resolved. The UNIQUE(channel, channel_external_id) constraint is what
-- guarantees a Meta wamid never produces two `message` rows even if the
-- vendor retries the same delivery five times.
--
-- processed_at is nullable to support a two-phase commit: row is INSERTed
-- inside the receiver's transaction (claiming the wamid), then updated
-- with processed_at = now() once the downstream `message` insert + wallet
-- debit succeed. A row with processed_at IS NULL after a window is a
-- crashed handler — GC inspects (received_at, processed_at) and
-- reprocesses or expires.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS inbound_message_dedup (
  id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  channel              text NOT NULL,
  channel_external_id  text NOT NULL,
  received_at          timestamptz NOT NULL DEFAULT now(),
  processed_at         timestamptz,
  CONSTRAINT inbound_message_dedup_channel_external_uniq
    UNIQUE (channel, channel_external_id)
);

CREATE INDEX IF NOT EXISTS inbound_message_dedup_received_idx
  ON inbound_message_dedup (received_at);
CREATE INDEX IF NOT EXISTS inbound_message_dedup_unprocessed_idx
  ON inbound_message_dedup (received_at)
  WHERE processed_at IS NULL;

ALTER TABLE inbound_message_dedup OWNER TO app_admin;

REVOKE ALL ON inbound_message_dedup FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON inbound_message_dedup TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON inbound_message_dedup TO app_master_ops;

DROP TRIGGER IF EXISTS inbound_message_dedup_master_ops_audit ON inbound_message_dedup;
CREATE TRIGGER inbound_message_dedup_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON inbound_message_dedup
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
