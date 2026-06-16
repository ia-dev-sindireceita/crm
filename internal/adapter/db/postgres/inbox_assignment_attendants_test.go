package postgres_test

// SIN-64978 integration tests for the inbox assignment adapters:
//   * AssignableAttendantRepository (ListAssignable / IsAssignable) — the
//     role + tenant gate behind the "assign to…" dropdown and the
//     AssignConversation use case.
//   * ConversationLeadStore (SetConversationLead) — keeps the read-model's
//     denormalised conversation.assigned_user_id coherent with the ledger.
//   * the ConversationReadModel UnassignedOnly filter (the inbox "fila").
//
// They live in the parent postgres_test package (not the pginbox
// subpackage) for the same reason as the rest of the inbox adapter tests:
// a separate test binary races the ALTER ROLE bootstrap on the shared CI
// cluster (memory note testpg_shared_cluster_race).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/inbox"
)

// seedUserWithRole inserts a tenant user with an explicit role and email,
// returning its id. The email local-part is the display label the adapter
// derives via inbox.UserLabelFromEmail.
func seedUserWithRole(t *testing.T, db *testpg.DB, tenantID uuid.UUID, role, localPart string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(newCtx(t),
		`INSERT INTO users (id, tenant_id, email, password_hash, role)
		 VALUES ($1, $2, $3, 'x', $4)`,
		id, tenantID, localPart+"@test", role,
	); err != nil {
		t.Fatalf("seed user (%s): %v", role, err)
	}
	return id
}

func TestInboxAdapter_ListAssignable_OnlyInboxRoles(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)

	atendente := seedUserWithRole(t, db, tenant, "tenant_atendente", "ana")
	gerente := seedUserWithRole(t, db, tenant, "tenant_gerente", "bia")
	_ = seedUserWithRole(t, db, tenant, "tenant_common", "caio") // not eligible
	_ = seedUserWithRole(t, db, tenant, "admin", "dora")         // not an inbox role

	got, err := store.ListAssignable(context.Background(), tenant)
	if err != nil {
		t.Fatalf("ListAssignable: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (atendente + gerente); got %+v", len(got), got)
	}
	// Ordered by email ascending: ana@ < bia@.
	if got[0].UserID != atendente || got[1].UserID != gerente {
		t.Errorf("order = [%v, %v], want [%v, %v]", got[0].UserID, got[1].UserID, atendente, gerente)
	}
	if got[0].DisplayName != "ana" || got[1].DisplayName != "bia" {
		t.Errorf("display names = [%q, %q], want [ana, bia]", got[0].DisplayName, got[1].DisplayName)
	}
}

func TestInboxAdapter_ListAssignable_EmptyTenant(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)

	got, err := store.ListAssignable(context.Background(), tenant)
	if err != nil {
		t.Fatalf("ListAssignable: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestInboxAdapter_ListAssignable_TenantIsolation(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	_ = seedUserWithRole(t, db, tenantB, "tenant_atendente", "eve")

	got, err := store.ListAssignable(context.Background(), tenantA)
	if err != nil {
		t.Fatalf("ListAssignable A: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("tenantA sees %d users, want 0 — RLS leak from tenantB", len(got))
	}
}

func TestInboxAdapter_IsAssignable(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	other := seedContactsTenant(t, db)

	atendente := seedUserWithRole(t, db, tenant, "tenant_atendente", "ana")
	gerente := seedUserWithRole(t, db, tenant, "tenant_gerente", "bia")
	common := seedUserWithRole(t, db, tenant, "tenant_common", "caio")
	crossTenant := seedUserWithRole(t, db, other, "tenant_atendente", "zoe")

	tests := []struct {
		name   string
		userID uuid.UUID
		want   bool
	}{
		{"atendente", atendente, true},
		{"gerente", gerente, true},
		{"common role rejected", common, false},
		{"unknown id rejected", uuid.New(), false},
		{"cross-tenant rejected by RLS", crossTenant, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := store.IsAssignable(context.Background(), tenant, tc.userID)
			if err != nil {
				t.Fatalf("IsAssignable: %v", err)
			}
			if ok != tc.want {
				t.Errorf("ok = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestInboxAdapter_IsAssignable_RejectsBadInput(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if _, err := store.IsAssignable(context.Background(), uuid.Nil, uuid.New()); err == nil {
		t.Error("nil tenant: want error")
	}
	ok, err := store.IsAssignable(context.Background(), seedContactsTenant(t, db), uuid.Nil)
	if err != nil || ok {
		t.Errorf("nil user: got (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

func TestInboxAdapter_SetConversationLead_Persists(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	user := seedUserWithRole(t, db, tenant, "tenant_atendente", "ana")

	conv, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	if err := store.SetConversationLead(context.Background(), tenant, conv.ID, user); err != nil {
		t.Fatalf("SetConversationLead: %v", err)
	}

	got, err := store.GetConversation(context.Background(), tenant, conv.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.AssignedUserID == nil || *got.AssignedUserID != user {
		t.Errorf("assigned_user_id = %v, want %v", got.AssignedUserID, user)
	}
}

func TestInboxAdapter_SetConversationLead_UnknownConversationNotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	user := seedUserWithRole(t, db, tenant, "tenant_atendente", "ana")

	err := store.SetConversationLead(context.Background(), tenant, uuid.New(), user)
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_SetConversationLead_CrossTenantHiddenByRLS(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	contactA := seedInboxContact(t, db, tenantA)
	userB := seedUserWithRole(t, db, tenantB, "tenant_atendente", "eve")

	conv, _ := inbox.NewConversation(tenantA, contactA.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// tenantB cannot touch tenantA's conversation — RLS hides the row, so
	// the UPDATE affects zero rows and collapses to ErrNotFound.
	err := store.SetConversationLead(context.Background(), tenantB, conv.ID, userB)
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("cross-tenant err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_ListConversationSummaries_UnassignedFilter(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	user := seedUserWithRole(t, db, tenant, "tenant_atendente", "ana")

	assigned, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), assigned); err != nil {
		t.Fatalf("CreateConversation assigned: %v", err)
	}
	unassigned, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), unassigned); err != nil {
		t.Fatalf("CreateConversation unassigned: %v", err)
	}
	if err := store.SetConversationLead(context.Background(), tenant, assigned.ID, user); err != nil {
		t.Fatalf("SetConversationLead: %v", err)
	}

	got, err := store.ListConversationSummaries(context.Background(), tenant, inbox.ConversationFilter{UnassignedOnly: true}, 10)
	if err != nil {
		t.Fatalf("ListConversationSummaries: %v", err)
	}
	if len(got) != 1 || got[0].ID != unassigned.ID {
		t.Fatalf("unassigned filter = %+v, want only the conversation with no lead", got)
	}
	if got[0].AssignedUserID != nil {
		t.Errorf("unassigned row AssignedUserID = %v, want nil", got[0].AssignedUserID)
	}
}
