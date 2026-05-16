package postgres_test

// SIN-62848 integration tests against a real Postgres for the
// receive-inbound → message.media={"scan_status":"pending"} flow that
// the Messenger handler (and the next round of Instagram fixes) depends
// on. The use-case wiring is shared between adapters; the assertions
// here cover the SQL behaviour the messenger.handler now expects from
// the materialiser:
//
//   - When InboundEvent.HasAttachments is true, the persisted message
//     row's `media` jsonb column carries scan_status="pending".
//   - When the carrier retries the same dedup key, the use case
//     reports Duplicate=true and the previously-persisted media row is
//     left alone (still scan_status="pending"); the operator never sees
//     an unscanned blob slip into the inbox (AC #1 + AC #4).
//   - MaterialiseInbound returns a non-nil MessageID on the first
//     delivery and a Nil MessageID on the duplicate, so the messenger
//     handler can fan out media.scan.requested envelopes against the
//     real row id (AC #2).

import (
	"context"
	"testing"

	"github.com/google/uuid"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

func TestInboxAdapter_Receive_HasAttachments_PersistsPendingMedia(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	contactsStore, err := pgcontacts.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgcontacts.New: %v", err)
	}
	contactsU := contactsusecase.MustNew(contactsStore)
	u := inboxusecase.MustNewReceiveInbound(store, store, contactsU)
	tenant := seedContactsTenant(t, db)
	ev := inbox.InboundEvent{
		TenantID:          tenant,
		Channel:           "messenger",
		ChannelExternalID: "mid.pending.1",
		SenderExternalID:  "PSID-pending",
		SenderDisplayName: "Pending Alice",
		Body:              "[image]",
		HasAttachments:    true,
	}

	res, err := u.MaterialiseInbound(context.Background(), ev)
	if err != nil {
		t.Fatalf("MaterialiseInbound: %v", err)
	}
	if res.Duplicate {
		t.Fatal("first delivery reported Duplicate")
	}
	if res.MessageID == uuid.Nil {
		t.Fatal("MaterialiseInbound returned uuid.Nil MessageID on first delivery")
	}

	var scanStatus string
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT media->>'scan_status' FROM message WHERE id = $1`, res.MessageID,
	).Scan(&scanStatus); err != nil {
		t.Fatalf("read message.media: %v", err)
	}
	if scanStatus != "pending" {
		t.Fatalf("scan_status = %q, want %q", scanStatus, "pending")
	}
}

func TestInboxAdapter_Receive_NoAttachments_LeavesMediaNull(t *testing.T) {
	// Regression guard: text-only inbound MUST NOT write a media row.
	// Persisting an empty media block would make the views projector
	// render a "scanning" placeholder for every plain text message.
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	contactsStore, err := pgcontacts.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgcontacts.New: %v", err)
	}
	contactsU := contactsusecase.MustNew(contactsStore)
	u := inboxusecase.MustNewReceiveInbound(store, store, contactsU)
	tenant := seedContactsTenant(t, db)
	ev := inbox.InboundEvent{
		TenantID:          tenant,
		Channel:           "messenger",
		ChannelExternalID: "mid.text.1",
		SenderExternalID:  "PSID-text",
		SenderDisplayName: "Plain Bob",
		Body:              "hello world",
		// HasAttachments deliberately left false.
	}

	res, err := u.MaterialiseInbound(context.Background(), ev)
	if err != nil {
		t.Fatalf("MaterialiseInbound: %v", err)
	}

	var hasMedia bool
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT media IS NOT NULL FROM message WHERE id = $1`, res.MessageID,
	).Scan(&hasMedia); err != nil {
		t.Fatalf("read message.media: %v", err)
	}
	if hasMedia {
		t.Fatal("text-only inbound persisted a non-null media column")
	}
}

// TestInboxAdapter_Receive_DuplicateLeavesPendingScanStatus is AC #4:
// a carrier retry against an already-processed dedup key must not
// disturb the message's scan_status. The first delivery materialises
// the row with scan_status="pending"; the second delivery returns
// Duplicate=true and the persisted row is read back untouched.
func TestInboxAdapter_Receive_DuplicateLeavesPendingScanStatus(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	contactsStore, err := pgcontacts.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgcontacts.New: %v", err)
	}
	contactsU := contactsusecase.MustNew(contactsStore)
	u := inboxusecase.MustNewReceiveInbound(store, store, contactsU)
	tenant := seedContactsTenant(t, db)
	ev := inbox.InboundEvent{
		TenantID:          tenant,
		Channel:           "messenger",
		ChannelExternalID: "mid.dup.1",
		SenderExternalID:  "PSID-dup",
		SenderDisplayName: "Dup Alice",
		Body:              "[image]",
		HasAttachments:    true,
	}

	first, err := u.MaterialiseInbound(context.Background(), ev)
	if err != nil {
		t.Fatalf("first Materialise: %v", err)
	}
	if first.Duplicate {
		t.Fatal("first delivery reported Duplicate")
	}
	if first.MessageID == uuid.Nil {
		t.Fatal("first delivery returned uuid.Nil")
	}

	second, err := u.MaterialiseInbound(context.Background(), ev)
	if err != nil {
		t.Fatalf("second Materialise: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("second delivery did not report Duplicate")
	}
	if second.MessageID != uuid.Nil {
		t.Fatalf("second delivery MessageID = %s, want uuid.Nil", second.MessageID)
	}

	var msgCount int
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT count(*) FROM message WHERE tenant_id = $1`, tenant,
	).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("messages = %d, want 1 after duplicate", msgCount)
	}

	var scanStatus string
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT media->>'scan_status' FROM message WHERE id = $1`, first.MessageID,
	).Scan(&scanStatus); err != nil {
		t.Fatalf("read message.media: %v", err)
	}
	if scanStatus != "pending" {
		t.Fatalf("scan_status after duplicate = %q, want %q", scanStatus, "pending")
	}
}
