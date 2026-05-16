// Integration tests for the F2-07.2 auto-attribution flow
// (SIN-62833): tenant.default_lead_user_id is consulted by the
// receive_inbound use-case and surfaces as an assignment_history row
// with reason='lead' on the freshly-created conversation.
//
// Lives in package postgres_test to share the TestMain harness and
// avoid a second test binary that would race ALTER ROLE on the
// shared CI cluster (memory note testpg_shared_cluster_race).
package postgres_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// freshDBWithDefaultLead applies the migration chain end-to-end for
// the F2-07.2 flow: tenants + users + inbox/contacts/dedup + identity
// link + assignment_history + the new default_lead_user_id column.
func freshDBWithDefaultLead(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0092_identity_link_assignment_history.up.sql",
		// 0094 adds message.media (jsonb) — SaveMessage now writes the
		// column unconditionally so even text-only inbound flows in this
		// fixture need the schema applied (SIN-62848).
		"0094_message_media_scan_status.up.sql",
		"0095_tenants_default_lead_user_id.up.sql",
	} {
		path := filepath.Join(harness.MigrationsDir(), name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	return db
}

// seedTenantWithDefaultLead inserts a tenant + a user belonging to that
// tenant, then sets tenants.default_lead_user_id to that user. Returns
// the (tenantID, leadUserID) pair. Admin pool bypasses RLS.
func seedTenantWithDefaultLead(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := newCtx(t)
	tenantID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, 'acme', $2)`,
		tenantID, tenantID.String()+".test",
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	userID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role)
		 VALUES ($1, $2, $3, 'x', 'agent')`,
		userID, tenantID, userID.String()+"@acme.test",
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE tenants SET default_lead_user_id = $1 WHERE id = $2`,
		userID, tenantID,
	); err != nil {
		t.Fatalf("update default_lead_user_id: %v", err)
	}
	return tenantID, userID
}

// seedTenantWithoutDefaultLead inserts a tenant with default_lead_user_id = NULL.
func seedTenantWithoutDefaultLead(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := newCtx(t)
	tenantID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, 'nolead', $2)`,
		tenantID, tenantID.String()+".nolead.test",
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return tenantID
}

// insertContactRow inserts the contact row the inbox.SaveMessage path
// will reference. The receive_inbound use-case persists the message
// against the conversation; the conversation FK requires a real
// contact row. The contacts adapter is exercised elsewhere, so we
// short-circuit by inserting the row directly via admin.
func insertContactRow(t *testing.T, pool *pgxpool.Pool, tenantID, contactID uuid.UUID) {
	t.Helper()
	ctx := newCtx(t)
	if _, err := pool.Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, 'int-test')`,
		contactID, tenantID,
	); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
}

// contactsBackfillUpserter is a small wrapper that returns a contact
// whose ID is already persisted in the contact table via admin —
// matching the production invariant (contacts adapter UPSERTs the row
// then returns it). The receive_inbound use-case reads contact.ID and
// uses it as conversation.contact_id; the FK fires on
// CreateConversation, so the row must exist before Execute runs.
type contactsBackfillUpserter struct {
	pool      *pgxpool.Pool
	tenantID  uuid.UUID
	contactID uuid.UUID
}

func (c *contactsBackfillUpserter) Execute(_ context.Context, in contactsusecase.Input) (contactsusecase.Result, error) {
	if in.TenantID != c.tenantID {
		return contactsusecase.Result{}, errors.New("contactsBackfillUpserter: tenant mismatch")
	}
	t := time.Now().UTC()
	ct := contacts.Hydrate(c.contactID, c.tenantID, in.DisplayName, nil, t, t)
	if err := ct.AddChannelIdentity(in.Channel, in.ExternalID); err != nil {
		return contactsusecase.Result{}, err
	}
	return contactsusecase.Result{Contact: ct, Created: true}, nil
}

// TestReceiveInbound_DefaultLead_EndToEnd is the AC bullet anchor: a
// tenant with default_lead_user_id set, an inbound event, and the
// resulting conversation MUST surface an assignment_history row with
// reason='lead' for the configured user. Read-back uses LatestAssignment
// (the canonical "who leads conversation X" query).
func TestReceiveInbound_DefaultLead_EndToEnd(t *testing.T) {
	db := freshDBWithDefaultLead(t)
	tenantID, leadUserID := seedTenantWithDefaultLead(t, db.AdminPool())
	contactID := uuid.New()
	insertContactRow(t, db.AdminPool(), tenantID, contactID)

	inboxStore, err := pginbox.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("inbox.New: %v", err)
	}
	tenantResolver, err := postgresadapter.NewTenantResolver(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewTenantResolver: %v", err)
	}
	contactsU := &contactsBackfillUpserter{pool: db.AdminPool(), tenantID: tenantID, contactID: contactID}

	u, err := inboxusecase.NewReceiveInboundWithLeadership(
		inboxStore, inboxStore, contactsU, tenantResolver, inboxStore,
	)
	if err != nil {
		t.Fatalf("NewReceiveInboundWithLeadership: %v", err)
	}

	res, err := u.Execute(newCtx(t), inbox.InboundEvent{
		TenantID:          tenantID,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.int.lead.1",
		SenderExternalID:  "+5511999990001",
		SenderDisplayName: "Alice",
		Body:              "hello",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Conversation == nil {
		t.Fatal("Conversation = nil")
	}

	// Round-trip through the adapter — the assertion is on the row
	// persisted under RLS, not the in-memory aggregate.
	latest, err := inboxStore.LatestAssignment(newCtx(t), tenantID, res.Conversation.ID)
	if err != nil {
		t.Fatalf("LatestAssignment: %v", err)
	}
	if latest.UserID != leadUserID {
		t.Errorf("LatestAssignment.UserID = %v, want %v", latest.UserID, leadUserID)
	}
	if latest.Reason != inbox.LeadReasonLead {
		t.Errorf("LatestAssignment.Reason = %q, want %q", latest.Reason, inbox.LeadReasonLead)
	}
}

// TestReceiveInbound_NoDefaultLead_EndToEnd covers "se ausente, fica
// sem líder": tenant exists, default_lead_user_id is NULL, no row in
// assignment_history is written.
func TestReceiveInbound_NoDefaultLead_EndToEnd(t *testing.T) {
	db := freshDBWithDefaultLead(t)
	tenantID := seedTenantWithoutDefaultLead(t, db.AdminPool())
	contactID := uuid.New()
	insertContactRow(t, db.AdminPool(), tenantID, contactID)

	inboxStore, err := pginbox.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("inbox.New: %v", err)
	}
	tenantResolver, err := postgresadapter.NewTenantResolver(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewTenantResolver: %v", err)
	}
	contactsU := &contactsBackfillUpserter{pool: db.AdminPool(), tenantID: tenantID, contactID: contactID}

	u, err := inboxusecase.NewReceiveInboundWithLeadership(
		inboxStore, inboxStore, contactsU, tenantResolver, inboxStore,
	)
	if err != nil {
		t.Fatalf("NewReceiveInboundWithLeadership: %v", err)
	}

	res, err := u.Execute(newCtx(t), inbox.InboundEvent{
		TenantID:          tenantID,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.int.nolead.1",
		SenderExternalID:  "+5511999990002",
		SenderDisplayName: "Bob",
		Body:              "hi",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	_, err = inboxStore.LatestAssignment(newCtx(t), tenantID, res.Conversation.ID)
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("LatestAssignment err = %v, want ErrNotFound (no row written)", err)
	}
}

// TestTenantsDefaultLeadUserID_FKSetNullOnUserDelete covers the FK
// ON DELETE SET NULL semantics in the migration: deleting the user
// must not block; the tenant's default_lead_user_id flips back to
// NULL.
func TestTenantsDefaultLeadUserID_FKSetNullOnUserDelete(t *testing.T) {
	db := freshDBWithDefaultLead(t)
	tenantID, leadUserID := seedTenantWithDefaultLead(t, db.AdminPool())
	ctx := newCtx(t)

	// Sanity: default_lead_user_id is set.
	var got *uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT default_lead_user_id FROM tenants WHERE id = $1`, tenantID).Scan(&got); err != nil {
		t.Fatalf("pre-delete read: %v", err)
	}
	if got == nil || *got != leadUserID {
		t.Fatalf("pre-delete default_lead_user_id = %v, want %v", got, leadUserID)
	}

	// Delete the user — FK ON DELETE SET NULL fires.
	if _, err := db.AdminPool().Exec(ctx, `DELETE FROM users WHERE id = $1`, leadUserID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	// Verify the tenant row is intact with default_lead_user_id = NULL.
	got = nil
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT default_lead_user_id FROM tenants WHERE id = $1`, tenantID).Scan(&got); err != nil {
		t.Fatalf("post-delete read: %v", err)
	}
	if got != nil {
		t.Errorf("post-delete default_lead_user_id = %v, want NULL", got)
	}
}

// TestTenantResolver_DefaultLeadUserID_ReadPath exercises the adapter
// method dedicated to the F2-07.2 read against real Postgres. Both
// the populated and the NULL paths are covered. Pairs with the
// unit-level Scan stubs in tenant_resolver_unit_test.go for the
// existing host/id-lookup methods.
func TestTenantResolver_DefaultLeadUserID_ReadPath(t *testing.T) {
	db := freshDBWithDefaultLead(t)
	tenantID, leadUserID := seedTenantWithDefaultLead(t, db.AdminPool())

	resolver, err := postgresadapter.NewTenantResolver(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewTenantResolver: %v", err)
	}
	ctx := newCtx(t)

	got, err := resolver.DefaultLeadUserID(ctx, tenantID)
	if err != nil {
		t.Fatalf("DefaultLeadUserID: %v", err)
	}
	if got == nil || *got != leadUserID {
		t.Errorf("DefaultLeadUserID = %v, want %v", got, leadUserID)
	}

	// Tenant without default_lead_user_id → nil, no error.
	otherTenant := seedTenantWithoutDefaultLead(t, db.AdminPool())
	got, err = resolver.DefaultLeadUserID(ctx, otherTenant)
	if err != nil {
		t.Fatalf("DefaultLeadUserID(nolead): %v", err)
	}
	if got != nil {
		t.Errorf("DefaultLeadUserID(nolead) = %v, want nil", got)
	}

	// Unknown tenant → ErrTenantNotFound (cannot confuse "missing" with
	// "no default").
	_, err = resolver.DefaultLeadUserID(ctx, uuid.New())
	if err == nil {
		t.Error("DefaultLeadUserID(unknown) err = nil, want ErrTenantNotFound")
	}

	// uuid.Nil → ErrTenantNotFound (same shape as ResolveByID).
	_, err = resolver.DefaultLeadUserID(ctx, uuid.Nil)
	if err == nil {
		t.Error("DefaultLeadUserID(uuid.Nil) err = nil, want ErrTenantNotFound")
	}
}
