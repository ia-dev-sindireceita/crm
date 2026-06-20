package postgres_test

// SIN-65392 integration tests for Store.DeleteMessagesByConversation.
//
// These live in the parent postgres_test package (not the
// internal/adapter/db/postgres/inbox subpackage) for the same reason as
// inbox_adapter_test.go: a separate test binary races the ALTER ROLE
// bootstrap on the shared CI cluster (SQLSTATE 28P01). We reuse the
// existing freshDBWithInboxContacts / seedContactsTenant / seedInboxContact
// helpers.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// seedMessages saves n inbound messages on conv and returns the conv id.
func seedMessagesForDelete(t *testing.T, store interface {
	SaveMessage(context.Context, *inbox.Message) error
}, tenant, conv uuid.UUID, n int) {
	t.Helper()
	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		m := inbox.HydrateMessage(uuid.New(), tenant, conv, inbox.MessageDirectionIn,
			"hi", inbox.MessageStatusDelivered, "", nil, base.Add(time.Duration(i)*time.Minute))
		if err := store.SaveMessage(context.Background(), m); err != nil {
			t.Fatalf("SaveMessage: %v", err)
		}
	}
}

func TestInboxAdapter_DeleteMessagesByConversation(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)

	conv, _ := inbox.NewConversation(tenant, contact.ID, "fakellm")
	if err := store.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	seedMessagesForDelete(t, store, tenant, conv.ID, 3)

	// Sanity: the parent pointer advanced on save.
	before, err := store.GetConversation(context.Background(), tenant, conv.ID)
	if err != nil {
		t.Fatalf("GetConversation before: %v", err)
	}
	if before.LastMessageAt.IsZero() {
		t.Fatal("precondition: LastMessageAt should be set after seeding messages")
	}

	deleted, err := store.DeleteMessagesByConversation(context.Background(), tenant, conv.ID)
	if err != nil {
		t.Fatalf("DeleteMessagesByConversation: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("deleted = %d, want 3", deleted)
	}

	msgs, err := store.ListMessages(context.Background(), tenant, conv.ID)
	if err != nil {
		t.Fatalf("ListMessages after delete: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("messages remaining = %d, want 0", len(msgs))
	}

	// The conversation row survives and its pointer is reset to NULL.
	after, err := store.GetConversation(context.Background(), tenant, conv.ID)
	if err != nil {
		t.Fatalf("GetConversation after: %v", err)
	}
	if !after.LastMessageAt.IsZero() {
		t.Fatalf("LastMessageAt = %v, want zero after reset", after.LastMessageAt)
	}
}

func TestInboxAdapter_DeleteMessagesByConversation_Idempotent(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)

	conv, _ := inbox.NewConversation(tenant, contact.ID, "fakellm")
	if err := store.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Empty thread → zero rows, no error.
	deleted, err := store.DeleteMessagesByConversation(context.Background(), tenant, conv.ID)
	if err != nil {
		t.Fatalf("DeleteMessagesByConversation on empty: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 on empty thread", deleted)
	}

	// Unknown conversation id → zero rows, no error.
	deleted, err = store.DeleteMessagesByConversation(context.Background(), tenant, uuid.New())
	if err != nil {
		t.Fatalf("DeleteMessagesByConversation on unknown id: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 on unknown id", deleted)
	}
}

func TestInboxAdapter_DeleteMessagesByConversation_TenantScoped(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)

	// Two tenants, each with their own fakellm conversation + messages.
	tenantA := seedContactsTenant(t, db)
	contactA := seedInboxContact(t, db, tenantA)
	convA, _ := inbox.NewConversation(tenantA, contactA.ID, "fakellm")
	if err := store.CreateConversation(context.Background(), convA); err != nil {
		t.Fatalf("CreateConversation A: %v", err)
	}
	seedMessagesForDelete(t, store, tenantA, convA.ID, 2)

	// A delete scoped to tenantA must NOT touch tenantB; and a delete that
	// passes tenantB's id with tenantA's conversation must remove nothing.
	tenantB := seedContactsTenant(t, db)

	deleted, err := store.DeleteMessagesByConversation(context.Background(), tenantB, convA.ID)
	if err != nil {
		t.Fatalf("cross-tenant delete: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("cross-tenant delete removed %d rows, want 0", deleted)
	}

	// tenantA's messages survive the cross-tenant attempt.
	msgs, err := store.ListMessages(context.Background(), tenantA, convA.ID)
	if err != nil {
		t.Fatalf("ListMessages A: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("tenantA messages = %d, want 2 (untouched by cross-tenant delete)", len(msgs))
	}

	// RejectsZeroTenant.
	if _, err := store.DeleteMessagesByConversation(context.Background(), uuid.Nil, convA.ID); err == nil {
		t.Fatal("zero tenant err = nil, want error")
	}
}
