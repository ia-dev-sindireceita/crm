package postgres_test

// SIN-65472 integration tests for the inbox reset delete-cascade:
// wiping a fakellm training thread must ALSO drop it back to Unassigned
// (visible through the LIVE ListConversationSummaries read path) and
// invalidate the cached ai_summary.
//
// These live in the parent postgres_test package (not the inbox/aiassist
// sub-packages) for the same reason as inbox_adapter_test.go and
// aiassist_adapter_test.go: a separate test binary races the ALTER ROLE
// bootstrap on the shared CI cluster (SQLSTATE 28P01). They reuse the
// existing seedContactsTenant / seedInboxContact / seedUserWithRole
// helpers and the testpg harness.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	aiassistpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/aiassist"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/aiassist"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// freshDBWithInboxAndAIAssist applies the migrations the reset cascade
// touches: tenants + users (FK targets and assignable atendentes),
// inbox_contacts (conversation/message/contact + assigned_user_id),
// the message media column, and 0098 (ai_summary). The chain matches
// production deploy order.
func freshDBWithInboxAndAIAssist(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0094_message_media_scan_status.up.sql",
		"0098_ai_policy_ai_summary_product_argument.up.sql",
	)
	return db
}

// inboxAssignmentClearer adapts *pginbox.Store.ClearConversationLead to
// inboxusecase.AssignmentClearer (the same shape cmd/server wires).
type inboxAssignmentClearer struct{ store *pginbox.Store }

func (c inboxAssignmentClearer) ClearAssignment(ctx context.Context, tenantID, conversationID uuid.UUID) error {
	return c.store.ClearConversationLead(ctx, tenantID, conversationID)
}

// aiSummaryInvalidator adapts the aiassist store's InvalidateForConversation
// to inboxusecase.SummaryInvalidator with a fixed clock for an
// auditable invalidated_at.
type aiSummaryInvalidator struct {
	store *aiassistpg.Store
	now   time.Time
}

func (s aiSummaryInvalidator) InvalidateSummaries(ctx context.Context, tenantID, conversationID uuid.UUID) error {
	return s.store.InvalidateForConversation(ctx, tenantID, conversationID, s.now)
}

func TestInboxReset_Cascade_ClearsAssignmentAndInvalidatesSummary(t *testing.T) {
	db := freshDBWithInboxAndAIAssist(t)
	store := newInboxStore(t, db)
	aiStore, err := aiassistpg.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("aiassistpg.New: %v", err)
	}
	ctx := context.Background()

	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	user := seedUserWithRole(t, db, tenant, "tenant_atendente", "ana")

	// A fakellm training conversation with messages, an assignee, and a
	// cached AI summary — the full state a reset must tear down.
	conv, _ := inbox.NewConversation(tenant, contact.ID, inboxusecase.TrainingChannel)
	if err := store.CreateConversation(ctx, conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	seedMessagesForDelete(t, store, tenant, conv.ID, 3)
	if err := store.SetConversationLead(ctx, tenant, conv.ID, user); err != nil {
		t.Fatalf("SetConversationLead: %v", err)
	}

	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	sm, err := aiassist.NewSummary(tenant, conv.ID, "stale summary", "google/gemini-2.0-flash", 100, 50, now, aiassist.DefaultSummaryTTL)
	if err != nil {
		t.Fatalf("NewSummary: %v", err)
	}
	if err := aiStore.Save(ctx, sm); err != nil {
		t.Fatalf("Save summary: %v", err)
	}
	// Precondition: the summary is live before the reset.
	if _, err := aiStore.GetLatestValid(ctx, tenant, conv.ID, now); err != nil {
		t.Fatalf("precondition GetLatestValid: %v", err)
	}

	uc := inboxusecase.MustNewResetConversation(
		store,
		inboxusecase.NoopConversationResetter{},
		inboxusecase.WithAssignmentClearer(inboxAssignmentClearer{store: store}),
		inboxusecase.WithSummaryInvalidator(aiSummaryInvalidator{store: aiStore, now: now}),
	)

	res, err := uc.Execute(ctx, inboxusecase.ResetConversationInput{TenantID: tenant, ConversationID: conv.ID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Deleted != 3 {
		t.Fatalf("Deleted = %d, want 3", res.Deleted)
	}

	// Messages gone.
	msgs, err := store.ListMessages(ctx, tenant, conv.ID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("messages remaining = %d, want 0", len(msgs))
	}

	// Assignment cleared — verified through the LIVE read path
	// (ListConversationSummaries reads conversation.assigned_user_id),
	// not just GetConversation.
	got, err := store.ListConversationSummaries(ctx, tenant, inbox.ConversationFilter{}, 50)
	if err != nil {
		t.Fatalf("ListConversationSummaries: %v", err)
	}
	var found bool
	for _, it := range got {
		if it.ID == conv.ID {
			found = true
			if it.AssignedUserID != nil {
				t.Fatalf("post-reset AssignedUserID = %v, want nil (Unassigned)", *it.AssignedUserID)
			}
		}
	}
	if !found {
		t.Fatalf("conversation %v missing from live summaries", conv.ID)
	}
	// And it now satisfies the Unassigned filter.
	un, err := store.ListConversationSummaries(ctx, tenant, inbox.ConversationFilter{UnassignedOnly: true}, 50)
	if err != nil {
		t.Fatalf("ListConversationSummaries unassigned: %v", err)
	}
	var inUnassigned bool
	for _, it := range un {
		if it.ID == conv.ID {
			inUnassigned = true
		}
	}
	if !inUnassigned {
		t.Fatalf("conversation %v not in Unassigned filter after reset", conv.ID)
	}

	// Summary invalidated — no valid summary survives the wipe.
	if _, err := aiStore.GetLatestValid(ctx, tenant, conv.ID, now); !errors.Is(err, aiassist.ErrCacheMiss) {
		t.Fatalf("post-reset GetLatestValid err = %v, want ErrCacheMiss", err)
	}
}

func TestInboxAdapter_ClearConversationLead_Persists(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	ctx := context.Background()
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	user := seedUserWithRole(t, db, tenant, "tenant_atendente", "ana")

	conv, _ := inbox.NewConversation(tenant, contact.ID, "fakellm")
	if err := store.CreateConversation(ctx, conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := store.SetConversationLead(ctx, tenant, conv.ID, user); err != nil {
		t.Fatalf("SetConversationLead: %v", err)
	}

	if err := store.ClearConversationLead(ctx, tenant, conv.ID); err != nil {
		t.Fatalf("ClearConversationLead: %v", err)
	}

	got, err := store.GetConversation(ctx, tenant, conv.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.AssignedUserID != nil {
		t.Fatalf("AssignedUserID = %v, want nil after clear", *got.AssignedUserID)
	}

	// Idempotent: clearing an already-Unassigned conversation succeeds.
	if err := store.ClearConversationLead(ctx, tenant, conv.ID); err != nil {
		t.Fatalf("ClearConversationLead (idempotent): %v", err)
	}
}

func TestInboxAdapter_ClearConversationLead_UnknownAndTenantScoped(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	ctx := context.Background()

	// Unknown id → ErrNotFound (no cross-tenant existence leak).
	tenant := seedContactsTenant(t, db)
	if err := store.ClearConversationLead(ctx, tenant, uuid.New()); !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("unknown id err = %v, want ErrNotFound", err)
	}

	// Cross-tenant: tenantB cannot clear tenantA's conversation (RLS hides
	// the row → zero rows → ErrNotFound), and the assignment survives.
	contactA := seedInboxContact(t, db, tenant)
	userA := seedUserWithRole(t, db, tenant, "tenant_atendente", "alice")
	convA, _ := inbox.NewConversation(tenant, contactA.ID, "fakellm")
	if err := store.CreateConversation(ctx, convA); err != nil {
		t.Fatalf("CreateConversation A: %v", err)
	}
	if err := store.SetConversationLead(ctx, tenant, convA.ID, userA); err != nil {
		t.Fatalf("SetConversationLead A: %v", err)
	}
	tenantB := seedContactsTenant(t, db)
	if err := store.ClearConversationLead(ctx, tenantB, convA.ID); !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("cross-tenant clear err = %v, want ErrNotFound", err)
	}
	gotA, err := store.GetConversation(ctx, tenant, convA.ID)
	if err != nil {
		t.Fatalf("GetConversation A: %v", err)
	}
	if gotA.AssignedUserID == nil || *gotA.AssignedUserID != userA {
		t.Fatalf("tenantA assignment = %v, want %v (untouched by cross-tenant clear)", gotA.AssignedUserID, userA)
	}

	// Reject zero tenant.
	if err := store.ClearConversationLead(ctx, uuid.Nil, convA.ID); err == nil {
		t.Fatal("zero tenant err = nil, want error")
	}
}
