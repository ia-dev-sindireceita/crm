package postgres_test

// SIN-65473 integration tests for the inbox ConversationStateStore adapter
// (SetConversationState) — the persistence behind the Encerrar / Reabrir
// conversa actions. They live in the parent postgres_test package (not the
// pginbox subpackage) for the same reason as the rest of the inbox adapter
// tests: a separate test binary races the ALTER ROLE bootstrap on the
// shared CI cluster (memory note testpg_shared_cluster_race).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// TestInboxAdapter_SetConversationState_RoundTrip closes then reopens a
// conversation through the adapter and reads the state back via
// GetConversation, proving the lifecycle column is persisted under tenant
// scope.
func TestInboxAdapter_SetConversationState_RoundTrip(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)

	conv, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Fresh conversations start open.
	got, err := store.GetConversation(context.Background(), tenant, conv.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.State != inbox.ConversationStateOpen {
		t.Fatalf("initial state = %q, want open", got.State)
	}

	// Close.
	if err := store.SetConversationState(context.Background(), tenant, conv.ID, inbox.ConversationStateClosed); err != nil {
		t.Fatalf("SetConversationState(closed): %v", err)
	}
	got, err = store.GetConversation(context.Background(), tenant, conv.ID)
	if err != nil {
		t.Fatalf("GetConversation after close: %v", err)
	}
	if got.State != inbox.ConversationStateClosed {
		t.Errorf("state after close = %q, want closed", got.State)
	}

	// Reopen.
	if err := store.SetConversationState(context.Background(), tenant, conv.ID, inbox.ConversationStateOpen); err != nil {
		t.Fatalf("SetConversationState(open): %v", err)
	}
	got, err = store.GetConversation(context.Background(), tenant, conv.ID)
	if err != nil {
		t.Fatalf("GetConversation after reopen: %v", err)
	}
	if got.State != inbox.ConversationStateOpen {
		t.Errorf("state after reopen = %q, want open", got.State)
	}
}

func TestInboxAdapter_SetConversationState_UnknownConversationNotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)

	err := store.SetConversationState(context.Background(), tenant, uuid.New(), inbox.ConversationStateClosed)
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_SetConversationState_CrossTenantHiddenByRLS(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	contactA := seedInboxContact(t, db, tenantA)

	conv, _ := inbox.NewConversation(tenantA, contactA.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// tenantB cannot touch tenantA's conversation — RLS hides the row, so
	// the UPDATE affects zero rows and collapses to ErrNotFound.
	err := store.SetConversationState(context.Background(), tenantB, conv.ID, inbox.ConversationStateClosed)
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("cross-tenant err = %v, want ErrNotFound", err)
	}

	// And tenantA's row is untouched.
	got, err := store.GetConversation(context.Background(), tenantA, conv.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.State != inbox.ConversationStateOpen {
		t.Errorf("tenantA state = %q, want open (cross-tenant write must not land)", got.State)
	}
}

func TestInboxAdapter_SetConversationState_RejectsZeroTenant(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if err := store.SetConversationState(context.Background(), uuid.Nil, uuid.New(), inbox.ConversationStateClosed); err == nil {
		t.Fatal("zero tenant: want error, got nil")
	}
}
