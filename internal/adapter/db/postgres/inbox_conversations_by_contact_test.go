package postgres_test

// SIN-64976 integration tests for the inbox Postgres Store's
// ListConversationsByContact read method, which backs the contact-detail
// conversation-history panel. The method is NOT part of the
// inbox.Repository port (only the concrete Store exposes it), mirroring
// the funnel Store.FindByID precedent.
//
// Parent postgres_test package for the shared-harness reason documented
// in inbox_adapter_test.go.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/inbox"
)

// seedContactWithEmail materialises a contact with a unique email identity
// so several contacts can coexist under one tenant without tripping the
// (channel, external_id) UNIQUE constraint.
func seedContactWithEmail(t *testing.T, db *testpg.DB, tenant uuid.UUID, name, email string) *contacts.Contact {
	t.Helper()
	store, err := pgcontacts.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgcontacts.New: %v", err)
	}
	c, err := contacts.New(tenant, name)
	if err != nil {
		t.Fatalf("contacts.New: %v", err)
	}
	if err := c.AddChannelIdentity("email", email); err != nil {
		t.Fatalf("AddChannelIdentity: %v", err)
	}
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return c
}

// seedConvWithLastMessage creates a conversation for contactID with an
// explicit last_message_at so the ORDER BY can be asserted deterministically.
func seedConvWithLastMessage(t *testing.T, store *pginbox.Store, tenant, contactID uuid.UUID, channel string, last time.Time) *inbox.Conversation {
	t.Helper()
	conv := inbox.HydrateConversation(uuid.New(), tenant, contactID, channel,
		inbox.ConversationStateOpen, nil, last, last)
	if err := store.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	return conv
}

func TestInboxAdapter_ListConversationsByContact_RejectsZeroTenant(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if _, err := store.ListConversationsByContact(context.Background(), uuid.Nil, uuid.New(), 10); err == nil {
		t.Error("ListConversationsByContact(nil tenant) err = nil")
	}
}

func TestInboxAdapter_ListConversationsByContact_NilContact(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	got, err := store.ListConversationsByContact(context.Background(), tenant, uuid.Nil, 10)
	if err != nil {
		t.Fatalf("ListConversationsByContact(nil contact): %v", err)
	}
	if got != nil {
		t.Errorf("got %d rows, want nil", len(got))
	}
}

func TestInboxAdapter_ListConversationsByContact_RejectsZeroLimit(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	if _, err := store.ListConversationsByContact(context.Background(), tenant, uuid.New(), 0); err == nil {
		t.Error("ListConversationsByContact(limit 0) err = nil")
	}
}

func TestInboxAdapter_ListConversationsByContact_OrderedNewestFirst(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedContactWithEmail(t, db, tenant, "Alice", "alice@example.com")
	other := seedContactWithEmail(t, db, tenant, "Bob", "bob@example.com")

	base := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	older := seedConvWithLastMessage(t, store, tenant, contact.ID, "whatsapp", base.Add(-2*time.Hour))
	newer := seedConvWithLastMessage(t, store, tenant, contact.ID, "email", base)
	// A conversation belonging to a different contact must not leak in.
	seedConvWithLastMessage(t, store, tenant, other.ID, "whatsapp", base.Add(time.Hour))

	got, err := store.ListConversationsByContact(context.Background(), tenant, contact.ID, 10)
	if err != nil {
		t.Fatalf("ListConversationsByContact: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2 (only Alice's threads)", len(got))
	}
	if got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Errorf("order = [%s, %s], want newest-first [%s, %s]", got[0].ID, got[1].ID, newer.ID, older.ID)
	}
}

func TestInboxAdapter_ListConversationsByContact_RespectsLimit(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedContactWithEmail(t, db, tenant, "Alice", "alice@example.com")
	base := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		seedConvWithLastMessage(t, store, tenant, contact.ID, "whatsapp", base.Add(time.Duration(i)*time.Hour))
	}
	got, err := store.ListConversationsByContact(context.Background(), tenant, contact.ID, 2)
	if err != nil {
		t.Fatalf("ListConversationsByContact: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("rows = %d, want 2 (limit)", len(got))
	}
}

func TestInboxAdapter_ListConversationsByContact_TenantScopedByRLS(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	contactA := seedContactWithEmail(t, db, tenantA, "Alice", "alice@example.com")
	base := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	seedConvWithLastMessage(t, store, tenantA, contactA.ID, "whatsapp", base)

	// Tenant B asking for tenant A's contact id sees nothing (RLS).
	got, err := store.ListConversationsByContact(context.Background(), tenantB, contactA.ID, 10)
	if err != nil {
		t.Fatalf("ListConversationsByContact: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("cross-tenant rows = %d, want 0", len(got))
	}
}
