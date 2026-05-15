package postgres_test

// SIN-62734 integration tests for the new
// FindMessageByChannelExternalID method on the inbox Postgres adapter
// and the UpdateMessageStatus use case (PR8 — WhatsApp status
// reconciler).
//
// These tests use the same testpg harness and helpers
// (freshDBWithInboxContacts, seedContactsTenant, seedInboxContact)
// declared in inbox_adapter_test.go; they live in the parent
// postgres_test package so the shared TestMain bootstrap covers
// them.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// seedOutboundMessageRow builds an out-direction message with a known
// wamid, persists it via the adapter, and returns its id. The result
// fixture lets every test in this file reuse a uniform set-up.
func seedOutboundMessageRow(t *testing.T, store *pginbox.Store, tenant, contactID uuid.UUID, wamid string, status inbox.MessageStatus) uuid.UUID {
	t.Helper()
	conv, _ := inbox.NewConversation(tenant, contactID, "whatsapp")
	if err := store.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	m, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:          tenant,
		ConversationID:    conv.ID,
		Direction:         inbox.MessageDirectionOut,
		Body:              "hi",
		Status:            status,
		ChannelExternalID: wamid,
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	return m.ID
}

func TestInboxAdapter_FindMessageByChannelExternalID(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	id := seedOutboundMessageRow(t, store, tenant, contact.ID, "wamid.find.1", inbox.MessageStatusSent)

	got, err := store.FindMessageByChannelExternalID(context.Background(), tenant, "whatsapp", "wamid.find.1")
	if err != nil {
		t.Fatalf("FindMessageByChannelExternalID: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID = %s, want %s", got.ID, id)
	}
	if got.ChannelExternalID != "wamid.find.1" {
		t.Errorf("ChannelExternalID = %q, want wamid.find.1", got.ChannelExternalID)
	}
	if got.Status != inbox.MessageStatusSent {
		t.Errorf("Status = %q, want sent", got.Status)
	}
	if got.TenantID != tenant {
		t.Errorf("TenantID = %s, want %s", got.TenantID, tenant)
	}
}

func TestInboxAdapter_FindMessageByChannelExternalID_NotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	if _, err := store.FindMessageByChannelExternalID(context.Background(), tenant, "whatsapp", "wamid.missing"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_FindMessageByChannelExternalID_RejectsZeroTenant(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if _, err := store.FindMessageByChannelExternalID(context.Background(), uuid.Nil, "whatsapp", "wamid.x"); err == nil {
		t.Error("zero tenant err = nil")
	}
}

func TestInboxAdapter_FindMessageByChannelExternalID_BlankInputsAreNotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	if _, err := store.FindMessageByChannelExternalID(context.Background(), tenant, "", "wamid.x"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("blank channel err = %v, want ErrNotFound", err)
	}
	if _, err := store.FindMessageByChannelExternalID(context.Background(), tenant, "whatsapp", ""); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("blank externalID err = %v, want ErrNotFound", err)
	}
}

// TestInboxAdapter_FindMessageByChannelExternalID_RLS verifies tenant
// isolation at the SQL layer — tenant B cannot read tenant A's
// message even if it knows the wamid. Mirrors
// TestInboxAdapter_GetConversation_CrossTenantHiddenByRLS for
// messages.
func TestInboxAdapter_FindMessageByChannelExternalID_RLS(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	contactA := seedInboxContact(t, db, tenantA)
	seedOutboundMessageRow(t, store, tenantA, contactA.ID, "wamid.tenA", inbox.MessageStatusSent)
	if _, err := store.FindMessageByChannelExternalID(context.Background(), tenantB, "whatsapp", "wamid.tenA"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("cross-tenant err = %v, want ErrNotFound", err)
	}
}

// TestInboxAdapter_UpdateMessageStatus_AdvancesAndPersists is the
// end-to-end integration test for AC #1 / #3: the use case wired to
// the real Postgres adapter advances sent → delivered → read and
// rejects regressions.
func TestInboxAdapter_UpdateMessageStatus_AdvancesAndPersists(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	seedOutboundMessageRow(t, store, tenant, contact.ID, "wamid.adv", inbox.MessageStatusPending)
	u := inboxusecase.MustNewUpdateMessageStatus(store, store)

	for _, s := range []inbox.MessageStatus{
		inbox.MessageStatusSent,
		inbox.MessageStatusDelivered,
		inbox.MessageStatusRead,
	} {
		res, err := u.HandleStatus(context.Background(), inbox.StatusUpdate{
			TenantID:          tenant,
			Channel:           "whatsapp",
			ChannelExternalID: "wamid.adv",
			NewStatus:         s,
		})
		if err != nil {
			t.Fatalf("HandleStatus(%s): %v", s, err)
		}
		if res.Outcome != inbox.StatusOutcomeApplied {
			t.Errorf("outcome(%s) = %q, want applied", s, res.Outcome)
		}
	}
	got, err := store.FindMessageByChannelExternalID(context.Background(), tenant, "whatsapp", "wamid.adv")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got.Status != inbox.MessageStatusRead {
		t.Fatalf("final status = %q, want read", got.Status)
	}

	// Regression: delivered after read is a no-op.
	res, err := u.HandleStatus(context.Background(), inbox.StatusUpdate{
		TenantID:          tenant,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.adv",
		NewStatus:         inbox.MessageStatusDelivered,
	})
	if err != nil {
		t.Fatalf("regression HandleStatus: %v", err)
	}
	if res.Outcome != inbox.StatusOutcomeNoop {
		t.Errorf("regression outcome = %q, want noop", res.Outcome)
	}
	got2, _ := store.FindMessageByChannelExternalID(context.Background(), tenant, "whatsapp", "wamid.adv")
	if got2.Status != inbox.MessageStatusRead {
		t.Errorf("after regression status = %q, want read (unchanged)", got2.Status)
	}
}

// TestInboxAdapter_UpdateMessageStatus_DedupReplayIsNoOp is the AC #2
// integration anchor: a replay of the same (wamid, status) pair
// collapses on the inbound_message_dedup UNIQUE constraint.
func TestInboxAdapter_UpdateMessageStatus_DedupReplayIsNoOp(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	seedOutboundMessageRow(t, store, tenant, contact.ID, "wamid.dup", inbox.MessageStatusPending)
	u := inboxusecase.MustNewUpdateMessageStatus(store, store)

	ev := inbox.StatusUpdate{
		TenantID:          tenant,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.dup",
		NewStatus:         inbox.MessageStatusSent,
	}
	first, err := u.HandleStatus(context.Background(), ev)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Outcome != inbox.StatusOutcomeApplied {
		t.Errorf("first outcome = %q, want applied", first.Outcome)
	}
	second, err := u.HandleStatus(context.Background(), ev)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Outcome != inbox.StatusOutcomeNoop {
		t.Errorf("second outcome = %q, want noop", second.Outcome)
	}
}
