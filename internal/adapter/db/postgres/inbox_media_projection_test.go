package postgres_test

// SIN-62805 F2-05d (R2) integration test: the inbox adapter SELECTs
// message.media and attaches it to inbox.Message so the inbox use-case
// layer can project MessageMediaView. The 0092 migration is applied by
// freshDBWithInboxContacts; here we exercise the read path end-to-end
// against a real postgres.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

func TestInboxAdapter_GetMessage_LoadsMediaJSONB(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	c, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("seed conv: %v", err)
	}
	m, _ := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenant, ConversationID: c.ID,
		Direction: inbox.MessageDirectionIn, Body: "image",
	})
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	// Patch media via the admin pool (mirrors the path the upload
	// worker takes — INSERT-time write + worker UpdateScanResult).
	if _, err := db.AdminPool().Exec(context.Background(),
		`UPDATE message SET media = $1::jsonb WHERE id = $2`,
		`{"hash":"deadbeef","format":"png","scan_status":"clean","scan_engine":"clamav-1.4"}`,
		m.ID,
	); err != nil {
		t.Fatalf("seed media: %v", err)
	}

	got, err := store.GetMessage(context.Background(), tenant, c.ID, m.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Media == nil {
		t.Fatal("Media: got nil want populated")
	}
	if got.Media.Hash != "deadbeef" || got.Media.Format != "png" || got.Media.ScanStatus != "clean" {
		t.Fatalf("Media: %+v", got.Media)
	}
}

func TestInboxAdapter_GetMessage_NoMediaLeavesNil(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	c, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("seed conv: %v", err)
	}
	m, _ := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenant, ConversationID: c.ID,
		Direction: inbox.MessageDirectionIn, Body: "text only",
	})
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	got, err := store.GetMessage(context.Background(), tenant, c.ID, m.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Media != nil {
		t.Fatalf("Media: got %+v want nil", got.Media)
	}
}

func TestInboxAdapter_ListMessages_LoadsMediaForInfected(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	c, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("seed conv: %v", err)
	}
	m, _ := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenant, ConversationID: c.ID,
		Direction: inbox.MessageDirectionIn, Body: "image",
	})
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if _, err := db.AdminPool().Exec(context.Background(),
		`UPDATE message SET media = $1::jsonb WHERE id = $2`,
		`{"hash":"abc","format":"jpg","scan_status":"infected","scan_engine":"clamav-1.4"}`,
		m.ID,
	); err != nil {
		t.Fatalf("seed media: %v", err)
	}
	msgs, err := store.ListMessages(context.Background(), tenant, c.ID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("ListMessages returned 0 messages")
	}
	var found *inbox.Message
	for _, mm := range msgs {
		if mm.ID == m.ID {
			found = mm
			break
		}
	}
	if found == nil || found.Media == nil {
		t.Fatalf("ListMessages: media not loaded: %+v", found)
	}
	if found.Media.ScanStatus != "infected" {
		t.Fatalf("ScanStatus: got %q want infected", found.Media.ScanStatus)
	}
}

// ensure the test does not regress when the conv is recreated by a new uuid
var _ = uuid.New
