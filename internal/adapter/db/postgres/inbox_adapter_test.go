package postgres_test

// SIN-62729 integration tests for the inbox Postgres adapter.
//
// These live in the parent postgres_test package (not the
// internal/adapter/db/postgres/inbox subpackage) for the same reason
// as contacts_adapter_test.go: shared TestMain / harness with the
// other postgres_test files. Tests that need testpg in a separate
// binary race the ALTER ROLE bootstrap on the shared CI cluster
// (SQLSTATE 28P01). The mastersession + contacts adapters use the
// same pattern; we follow it here.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

func newInboxStore(t *testing.T, db *testpg.DB) *pginbox.Store {
	t.Helper()
	s, err := pginbox.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pginbox.New: %v", err)
	}
	return s
}

// seedInboxContact reuses the contacts adapter to materialise a real
// row that satisfies the conversation/message FKs.
func seedInboxContact(t *testing.T, db *testpg.DB, tenantID uuid.UUID) *contacts.Contact {
	t.Helper()
	store, err := pgcontacts.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgcontacts.New: %v", err)
	}
	c, err := contacts.New(tenantID, "Alice")
	if err != nil {
		t.Fatalf("contacts.New: %v", err)
	}
	if err := c.AddChannelIdentity(contacts.ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("AddChannelIdentity: %v", err)
	}
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return c
}

func TestInboxAdapter_New_RejectsNilPool(t *testing.T) {
	if _, err := pginbox.New(nil); err == nil {
		t.Error("New(nil) err = nil")
	}
}

// TestInboxAdapter_WithClock_AppliesToZeroTimestamps verifies that the
// adapter pins zero-value CreatedAt fields to the configured clock.
// Both CreateConversation and SaveMessage exercise the clock when
// callers pass a Hydrate-built aggregate with zero timestamps.
func TestInboxAdapter_WithClock_AppliesToZeroTimestamps(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	pinned := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	store := newInboxStore(t, db).WithClock(func() time.Time { return pinned })
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	conv := inbox.HydrateConversation(uuid.New(), tenant, contact.ID, "whatsapp",
		inbox.ConversationStateOpen, nil, time.Time{}, time.Time{})
	if err := store.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	got, err := store.GetConversation(context.Background(), tenant, conv.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if !got.CreatedAt.Equal(pinned) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, pinned)
	}
	// Also make SaveMessage exercise the clock.
	m := inbox.HydrateMessage(uuid.New(), tenant, conv.ID, inbox.MessageDirectionIn,
		"hi", inbox.MessageStatusDelivered, "", nil, time.Time{})
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if !m.CreatedAt.Equal(pinned) {
		t.Errorf("Message.CreatedAt = %v, want %v", m.CreatedAt, pinned)
	}
}

func TestInboxAdapter_CreateAndGetConversation(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)

	c, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	got, err := store.GetConversation(context.Background(), tenant, c.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.ID != c.ID || got.TenantID != tenant || got.ContactID != contact.ID ||
		got.Channel != "whatsapp" || got.State != inbox.ConversationStateOpen {
		t.Errorf("Conversation mismatch: %+v", got)
	}
}

func TestInboxAdapter_GetConversation_NotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	_, err := store.GetConversation(context.Background(), tenant, uuid.New())
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_GetConversation_RejectsZeroTenant(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if _, err := store.GetConversation(context.Background(), uuid.Nil, uuid.New()); err == nil {
		t.Error("zero tenant err = nil")
	}
}

func TestInboxAdapter_GetConversation_NilIDIsNotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	if _, err := store.GetConversation(context.Background(), tenant, uuid.Nil); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_GetConversation_CrossTenantHiddenByRLS(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	contactA := seedInboxContact(t, db, tenantA)
	c, _ := inbox.NewConversation(tenantA, contactA.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.GetConversation(context.Background(), tenantB, c.ID); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("cross-tenant err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_FindOpenConversation(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	// Initially no open conversation.
	if _, err := store.FindOpenConversation(context.Background(), tenant, contact.ID, "whatsapp"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	c, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	got, err := store.FindOpenConversation(context.Background(), tenant, contact.ID, "whatsapp")
	if err != nil {
		t.Fatalf("FindOpenConversation: %v", err)
	}
	if got.ID != c.ID {
		t.Errorf("ID = %s, want %s", got.ID, c.ID)
	}
	// Closing it removes it from FindOpenConversation's view.
	if _, err := db.AdminPool().Exec(context.Background(),
		`UPDATE conversation SET state = 'closed' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := store.FindOpenConversation(context.Background(), tenant, contact.ID, "whatsapp"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("after close err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_FindOpenConversation_RejectsZeroTenant(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if _, err := store.FindOpenConversation(context.Background(), uuid.Nil, uuid.New(), "whatsapp"); err == nil {
		t.Error("zero tenant err = nil")
	}
}

func TestInboxAdapter_FindOpenConversation_NilContactIsNotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	if _, err := store.FindOpenConversation(context.Background(), tenant, uuid.Nil, "whatsapp"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_SaveMessage_BumpsLastMessageAt(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	c, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	m, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenant, ConversationID: c.ID,
		Direction: inbox.MessageDirectionIn, Body: "hi",
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	got, err := store.GetConversation(context.Background(), tenant, c.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if !got.LastMessageAt.Equal(m.CreatedAt.UTC().Truncate(time.Microsecond)) {
		// pg stores microsecond precision; equality after round-trip
		// is microsecond-accurate but not exact-nanosecond.
		if got.LastMessageAt.Sub(m.CreatedAt) > time.Millisecond {
			t.Errorf("LastMessageAt = %v, want ~%v", got.LastMessageAt, m.CreatedAt)
		}
	}
}

func TestInboxAdapter_SaveMessage_RejectsNilOrZero(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if err := store.SaveMessage(context.Background(), nil); err == nil {
		t.Error("nil err = nil")
	}
	if err := store.SaveMessage(context.Background(), &inbox.Message{}); err == nil {
		t.Error("zero err = nil")
	}
	if err := store.SaveMessage(context.Background(), &inbox.Message{TenantID: uuid.New()}); err == nil {
		t.Error("missing ids err = nil")
	}
}

func TestInboxAdapter_UpdateMessage(t *testing.T) {
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
		Direction: inbox.MessageDirectionOut, Body: "hi",
	})
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if err := m.AdvanceStatus(inbox.MessageStatusSent); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if err := m.AttachChannelExternalID("wamid.xyz"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := store.UpdateMessage(context.Background(), m); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}
	// Verify via admin pool (RLS-bypass).
	var status, externalID string
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT status, channel_external_id FROM message WHERE id = $1`,
		m.ID).Scan(&status, &externalID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if status != "sent" || externalID != "wamid.xyz" {
		t.Errorf("status=%q externalID=%q, want sent/wamid.xyz", status, externalID)
	}
}

func TestInboxAdapter_UpdateMessage_NotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	m := inbox.HydrateMessage(uuid.New(), tenant, uuid.New(),
		inbox.MessageDirectionOut, "hi", inbox.MessageStatusSent, "wamid.x", nil, time.Now().UTC())
	if err := store.UpdateMessage(context.Background(), m); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_UpdateMessage_RejectsZeroTenantOrID(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if err := store.UpdateMessage(context.Background(), nil); err == nil {
		t.Error("nil err = nil")
	}
	if err := store.UpdateMessage(context.Background(), &inbox.Message{}); err == nil {
		t.Error("zero tenant err = nil")
	}
	if err := store.UpdateMessage(context.Background(),
		&inbox.Message{TenantID: uuid.New()}); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("zero id err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_GetMessage_HappyPath(t *testing.T) {
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
		Direction: inbox.MessageDirectionOut, Body: "hi",
		Status: inbox.MessageStatusPending,
	})
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	got, err := store.GetMessage(context.Background(), tenant, c.ID, m.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.ID != m.ID || got.ConversationID != c.ID || got.Status != inbox.MessageStatusPending {
		t.Fatalf("got=%+v want id=%s conv=%s status=pending", got, m.ID, c.ID)
	}
}

func TestInboxAdapter_GetMessage_NotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	_, err := store.GetMessage(context.Background(), tenant, uuid.New(), uuid.New())
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_GetMessage_RejectsZeroes(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if _, err := store.GetMessage(context.Background(), uuid.Nil, uuid.New(), uuid.New()); err == nil {
		t.Errorf("nil tenant should error")
	}
	if _, err := store.GetMessage(context.Background(), uuid.New(), uuid.Nil, uuid.New()); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("nil conversation should map to ErrNotFound, got %v", err)
	}
	if _, err := store.GetMessage(context.Background(), uuid.New(), uuid.New(), uuid.Nil); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("nil message id should map to ErrNotFound, got %v", err)
	}
}

func TestInboxAdapter_GetMessage_WrongConversationIsNotFound(t *testing.T) {
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
		Direction: inbox.MessageDirectionOut, Body: "hi",
	})
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	_, err := store.GetMessage(context.Background(), tenant, uuid.New(), m.ID)
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_GetMessage_CrossTenantIsHiddenByRLS(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenantA := seedContactsTenant(t, db)
	contactA := seedInboxContact(t, db, tenantA)
	convA, _ := inbox.NewConversation(tenantA, contactA.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), convA); err != nil {
		t.Fatalf("seed conv: %v", err)
	}
	m, _ := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenantA, ConversationID: convA.ID,
		Direction: inbox.MessageDirectionOut, Body: "hi",
	})
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	tenantB := seedContactsTenant(t, db)
	_, err := store.GetMessage(context.Background(), tenantB, convA.ID, m.ID)
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestInboxAdapter_Claim_AndMarkProcessed(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	ctx := context.Background()
	if err := store.Claim(ctx, "whatsapp", "wamid.dedup-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Claim(ctx, "whatsapp", "wamid.dedup-1"); !errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
		t.Errorf("second Claim err = %v, want ErrInboundAlreadyProcessed", err)
	}
	if err := store.MarkProcessed(ctx, "whatsapp", "wamid.dedup-1"); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	// processed_at present.
	var processed *time.Time
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT processed_at FROM inbound_message_dedup WHERE channel='whatsapp' AND channel_external_id='wamid.dedup-1'`).Scan(&processed); err != nil {
		t.Fatalf("verify processed: %v", err)
	}
	if processed == nil {
		t.Error("processed_at = nil after MarkProcessed")
	}
}

func TestInboxAdapter_Claim_RejectsBlank(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if err := store.Claim(context.Background(), "", "wamid.x"); err == nil {
		t.Error("blank channel err = nil")
	}
	if err := store.Claim(context.Background(), "whatsapp", ""); err == nil {
		t.Error("blank externalID err = nil")
	}
}

func TestInboxAdapter_MarkProcessed_RejectsBlank(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if err := store.MarkProcessed(context.Background(), "", "wamid.x"); err == nil {
		t.Error("blank channel err = nil")
	}
	if err := store.MarkProcessed(context.Background(), "whatsapp", ""); err == nil {
		t.Error("blank externalID err = nil")
	}
}

func TestInboxAdapter_MarkProcessed_NotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if err := store.MarkProcessed(context.Background(), "whatsapp", "missing"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestInboxAdapter_ReceiveInbound_Idempotent is AC #4: two calls
// through the use-case with the same dedup key result in exactly 1
// persisted message and 1 created contact.
func TestInboxAdapter_ReceiveInbound_Idempotent(t *testing.T) {
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
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.acc4",
		SenderExternalID:  "+5511999990001",
		SenderDisplayName: "Alice",
		Body:              "hello",
	}
	first, err := u.Execute(context.Background(), ev)
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if first.Duplicate {
		t.Error("first call reported Duplicate")
	}
	second, err := u.Execute(context.Background(), ev)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if !second.Duplicate {
		t.Error("second call did not report Duplicate")
	}
	var msgs int
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT count(*) FROM message WHERE tenant_id = $1`, tenant).Scan(&msgs); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgs != 1 {
		t.Errorf("messages = %d, want 1", msgs)
	}
	var contactsCount int
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT count(*) FROM contact WHERE tenant_id = $1`, tenant).Scan(&contactsCount); err != nil {
		t.Fatalf("count contacts: %v", err)
	}
	if contactsCount != 1 {
		t.Errorf("contacts = %d, want 1", contactsCount)
	}
}

// TestInboxAdapter_ReceiveInbound_ConcurrentIdempotent races 100
// callers on the same dedup key against the real Postgres UNIQUE
// constraint. Exactly one message + one contact must end up
// persisted; the other 99 callers report Duplicate.
//
// This is the AC#2 anchor for SIN-62731 ("Idempotência testada:
// replay do mesmo wamid 100x concorrente → 1 message persistida no
// DB"). The whatsapp adapter test
// `TestPost_ConcurrentReplay_OneMessage` covers the HTTP fan-out side
// with a mutex-guarded in-memory fake; this test covers the
// `inbound_message_dedup(channel, channel_external_id)` UNIQUE
// constraint at the SQL layer, which the in-memory fake by
// construction cannot reach.
func TestInboxAdapter_ReceiveInbound_ConcurrentIdempotent(t *testing.T) {
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
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.conc",
		SenderExternalID:  "+5511999990001",
		SenderDisplayName: "Alice",
		Body:              "hello",
	}
	const n = 100
	var wg sync.WaitGroup
	var dups atomic.Int64
	var firstErr atomic.Value
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := u.Execute(ctx, ev)
			if err != nil {
				firstErr.CompareAndSwap(nil, err)
				return
			}
			if res.Duplicate {
				dups.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if v := firstErr.Load(); v != nil {
		t.Fatalf("concurrent Execute: %v", v)
	}
	if got := dups.Load(); got != n-1 {
		t.Errorf("Duplicate count = %d, want %d", got, n-1)
	}
	var msgs int
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT count(*) FROM message WHERE tenant_id = $1`, tenant).Scan(&msgs); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgs != 1 {
		t.Errorf("messages = %d, want 1", msgs)
	}
	// AC#2 also requires exactly 1 contact persisted (one inbound
	// sender, deduped via contacts.UpsertContactByChannel + the
	// dedup ledger). Asserting the count here closes the gap between
	// "concurrent callers" and "Postgres-side uniqueness".
	var contacts int
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT count(*) FROM contact WHERE tenant_id = $1`, tenant).Scan(&contacts); err != nil {
		t.Fatalf("count contacts: %v", err)
	}
	if contacts != 1 {
		t.Errorf("contacts = %d, want 1", contacts)
	}
	var dedupRows int
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT count(*) FROM inbound_message_dedup WHERE channel = $1 AND channel_external_id = $2`,
		ev.Channel, ev.ChannelExternalID).Scan(&dedupRows); err != nil {
		t.Fatalf("count dedup rows: %v", err)
	}
	if dedupRows != 1 {
		t.Errorf("inbound_message_dedup rows = %d, want 1", dedupRows)
	}
}

// TestInboxAdapter_SendOutbound_DebitsWalletAtZeroCost is AC #5: the
// debit/commit path runs even when cost == 0. We use a documented
// in-memory wallet that mirrors the WalletDebitor contract (PR5 ships
// the Postgres wallet adapter); inbox PR4 only depends on the port.
func TestInboxAdapter_SendOutbound_DebitsWalletAtZeroCost(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	c, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	wallet := newCountingWallet()
	outbound := newFixedOutbound("wamid.zero")
	u := inboxusecase.MustNewSendOutbound(store, wallet, outbound)
	res, err := u.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       tenant,
		ConversationID: c.ID,
		Body:           "hi",
		ToExternalID:   "+5511999990001",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Message == nil || res.Message.Status != inbox.MessageStatusSent {
		t.Fatalf("Message status = %+v, want sent", res.Message)
	}
	if wallet.debits != 1 {
		t.Errorf("wallet.debits = %d, want 1", wallet.debits)
	}
	if wallet.commits != 1 {
		t.Errorf("wallet.commits = %d, want 1", wallet.commits)
	}
	if wallet.refunds != 0 {
		t.Errorf("wallet.refunds = %d, want 0", wallet.refunds)
	}
	if wallet.balance[tenant] != 0 {
		t.Errorf("balance = %d, want 0", wallet.balance[tenant])
	}
	// Persisted row reflects the sent status.
	var status, externalID string
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT status, channel_external_id FROM message WHERE id = $1`, res.Message.ID).Scan(&status, &externalID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if status != "sent" || externalID != "wamid.zero" {
		t.Errorf("status=%q externalID=%q, want sent/wamid.zero", status, externalID)
	}
}

// TestInboxAdapter_GetConversation_AfterRecordAndReopen exercises the
// Hydrate-path on a conversation that has been transitioned and has a
// non-zero LastMessageAt.
func TestInboxAdapter_GetConversation_AfterRecordAndReopen(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	c, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	m, _ := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenant, ConversationID: c.ID,
		Direction: inbox.MessageDirectionIn, Body: "ping",
	})
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if _, err := db.AdminPool().Exec(context.Background(),
		`UPDATE conversation SET state='closed' WHERE id=$1`, c.ID); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, err := store.GetConversation(context.Background(), tenant, c.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.State != inbox.ConversationStateClosed {
		t.Errorf("State = %q, want closed", got.State)
	}
	if got.LastMessageAt.IsZero() {
		t.Error("LastMessageAt is zero after SaveMessage")
	}
}

// TestInboxAdapter_CreateConversation_RejectsZeroes covers the
// boundary validations on CreateConversation.
func TestInboxAdapter_CreateConversation_RejectsZeroes(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if err := store.CreateConversation(context.Background(), nil); err == nil {
		t.Error("nil err = nil")
	}
	if err := store.CreateConversation(context.Background(), &inbox.Conversation{}); err == nil {
		t.Error("zero tenant err = nil")
	}
	if err := store.CreateConversation(context.Background(),
		&inbox.Conversation{TenantID: uuid.New()}); err == nil {
		t.Error("zero conversation id err = nil")
	}
}

// TestInboxAdapter_RLS_TenantIsolation checks that an existing
// conversation under tenant A is invisible to tenant B's runtime
// connection.
func TestInboxAdapter_RLS_TenantIsolation(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	contactA := seedInboxContact(t, db, tenantA)
	c, _ := inbox.NewConversation(tenantA, contactA.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	// Tenant B should not be able to see it via FindOpenConversation.
	if _, err := store.FindOpenConversation(context.Background(), tenantB, contactA.ID, "whatsapp"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("cross-tenant find err = %v, want ErrNotFound", err)
	}
}

// TestInboxAdapter_SaveMessage_HonorsRLS confirms that a SaveMessage
// under tenant A cannot be observed by a separate runtime read under
// tenant B even if B knows the message id.
func TestInboxAdapter_SaveMessage_HonorsRLS(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	contactA := seedInboxContact(t, db, tenantA)
	c, _ := inbox.NewConversation(tenantA, contactA.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	m, _ := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenantA, ConversationID: c.ID,
		Direction: inbox.MessageDirectionIn, Body: "ping",
	})
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	// Read as tenant B via WithTenant → RLS hides the row.
	var seen int
	if err := postgresadapter.WithTenant(context.Background(), db.RuntimePool(), tenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM message WHERE id = $1`, m.ID).Scan(&seen)
	}); err != nil {
		t.Fatalf("read as B: %v", err)
	}
	if seen != 0 {
		t.Errorf("tenant B saw %d messages, want 0", seen)
	}
}

// TestInboxAdapter_ListConversations_OrderedAndScoped exercises the
// SIN-62735 PR9 list path: ordering is newest-last-message-first,
// tenant isolation comes from RLS, and the optional state filter
// restricts to open/closed when supplied.
func TestInboxAdapter_ListConversations_OrderedAndScoped(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	contactA := seedInboxContact(t, db, tenantA)

	older, _ := inbox.NewConversation(tenantA, contactA.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), older); err != nil {
		t.Fatalf("CreateConversation older: %v", err)
	}
	newer, _ := inbox.NewConversation(tenantA, contactA.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), newer); err != nil {
		t.Fatalf("CreateConversation newer: %v", err)
	}
	// Bump last_message_at via SaveMessage so the ORDER BY has a basis.
	mOlder, _ := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenantA, ConversationID: older.ID,
		Direction: inbox.MessageDirectionIn, Body: "older",
	})
	if err := store.SaveMessage(context.Background(), mOlder); err != nil {
		t.Fatalf("SaveMessage older: %v", err)
	}
	mNewer, _ := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenantA, ConversationID: newer.ID,
		Direction: inbox.MessageDirectionIn, Body: "newer",
	})
	if err := store.SaveMessage(context.Background(), mNewer); err != nil {
		t.Fatalf("SaveMessage newer: %v", err)
	}
	// Close the older conversation to exercise the state filter.
	if _, err := db.AdminPool().Exec(context.Background(),
		`UPDATE conversation SET state = 'closed' WHERE id = $1`, older.ID); err != nil {
		t.Fatalf("close older: %v", err)
	}

	got, err := store.ListConversations(context.Background(), tenantA, "", 10)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != newer.ID {
		t.Errorf("first row = %s, want %s (newer)", got[0].ID, newer.ID)
	}

	openOnly, err := store.ListConversations(context.Background(), tenantA, inbox.ConversationStateOpen, 10)
	if err != nil {
		t.Fatalf("ListConversations open: %v", err)
	}
	if len(openOnly) != 1 || openOnly[0].ID != newer.ID {
		t.Errorf("open filter = %+v, want [newer]", openOnly)
	}

	// Cross-tenant: tenant B sees nothing of tenant A's rows under RLS.
	others, err := store.ListConversations(context.Background(), tenantB, "", 10)
	if err != nil {
		t.Fatalf("ListConversations B: %v", err)
	}
	if len(others) != 0 {
		t.Errorf("tenant B saw %d rows, want 0", len(others))
	}
}

// TestInboxAdapter_ListConversations_RejectsBadInput covers the
// validation branches.
func TestInboxAdapter_ListConversations_RejectsBadInput(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if _, err := store.ListConversations(context.Background(), uuid.Nil, "", 10); err == nil {
		t.Error("zero tenant err = nil")
	}
	tenant := seedContactsTenant(t, db)
	if _, err := store.ListConversations(context.Background(), tenant, "", 0); err == nil {
		t.Error("limit=0 err = nil")
	}
	// Limit clamp: an absurd page does not error and is silently capped.
	if _, err := store.ListConversations(context.Background(), tenant, "", 999999); err != nil {
		t.Errorf("clamp err = %v", err)
	}
}

// TestInboxAdapter_ListMessages_ReturnsOrderedThread covers the
// SIN-62735 PR9 conversation view path: messages return oldest-first.
func TestInboxAdapter_ListMessages_ReturnsOrderedThread(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)

	c, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	m1, _ := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenant, ConversationID: c.ID,
		Direction: inbox.MessageDirectionIn, Body: "first",
	})
	if err := store.SaveMessage(context.Background(), m1); err != nil {
		t.Fatalf("SaveMessage first: %v", err)
	}
	m2, _ := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenant, ConversationID: c.ID,
		Direction: inbox.MessageDirectionOut, Body: "second",
	})
	if err := store.SaveMessage(context.Background(), m2); err != nil {
		t.Fatalf("SaveMessage second: %v", err)
	}

	got, err := store.ListMessages(context.Background(), tenant, c.ID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Body != "first" || got[1].Body != "second" {
		t.Errorf("order: %+v", got)
	}
}

// TestInboxAdapter_ListMessages_NotFoundOnUnknownConversation covers
// the RLS-hidden / non-existent paths.
func TestInboxAdapter_ListMessages_NotFoundOnUnknownConversation(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	if _, err := store.ListMessages(context.Background(), tenant, uuid.New()); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("unknown id err = %v, want ErrNotFound", err)
	}
	if _, err := store.ListMessages(context.Background(), tenant, uuid.Nil); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("nil id err = %v, want ErrNotFound", err)
	}
	if _, err := store.ListMessages(context.Background(), uuid.Nil, uuid.New()); err == nil {
		t.Error("zero tenant err = nil")
	}
}

// countingWallet is a documented in-memory implementation of
// inbox.WalletDebitor. PR5 (SIN-62727) will ship the Postgres wallet
// adapter and the production composition root will swap it in; PR4
// integration tests use this fake to verify SendOutbound exercises
// the reserve / commit / refund bookkeeping (AC #5).
type countingWallet struct {
	mu      sync.Mutex
	balance map[uuid.UUID]int64
	debits  int
	commits int
	refunds int
}

func newCountingWallet() *countingWallet {
	return &countingWallet{balance: map[uuid.UUID]int64{}}
}

func (w *countingWallet) Debit(ctx context.Context, tenantID uuid.UUID, cost int64, charge func(ctx context.Context) error) error {
	w.mu.Lock()
	w.debits++
	w.balance[tenantID] -= cost
	w.mu.Unlock()
	if err := charge(ctx); err != nil {
		w.mu.Lock()
		w.refunds++
		w.balance[tenantID] += cost
		w.mu.Unlock()
		return err
	}
	w.mu.Lock()
	w.commits++
	w.mu.Unlock()
	return nil
}

// fixedOutbound implements inbox.OutboundChannel by returning a
// configured channel-external-id. err, if set, fails SendMessage so
// failure paths can be tested.
type fixedOutbound struct {
	channelExternalID string
	err               error
}

func newFixedOutbound(id string) *fixedOutbound {
	return &fixedOutbound{channelExternalID: id}
}

func (o *fixedOutbound) SendMessage(_ context.Context, _ inbox.OutboundMessage) (string, error) {
	if o.err != nil {
		return "", o.err
	}
	return o.channelExternalID, nil
}

// ---------------------------------------------------------------------------
// SIN-64967 — read-model: ListConversationSummaries + UserDirectory.
// These exercise the GET /inbox list pane read side: last-message snippet
// + direction (single lateral query), the channel/assigned filters, tenant
// isolation, and the atendente-label directory.
// ---------------------------------------------------------------------------

// hydrateAndSave persists a message with an explicit created_at so the
// lateral "latest message" lookup has a deterministic ordering basis.
func hydrateAndSave(t *testing.T, store *pginbox.Store, tenant, convID uuid.UUID, dir inbox.MessageDirection, body string, at time.Time) {
	t.Helper()
	status := inbox.MessageStatusDelivered
	if dir == inbox.MessageDirectionOut {
		status = inbox.MessageStatusSent
	}
	m := inbox.HydrateMessage(uuid.New(), tenant, convID, dir, body, status, "", nil, at)
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
}

func TestInboxAdapter_ListConversationSummaries_SnippetAndDirection(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)

	withMsgs, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), withMsgs); err != nil {
		t.Fatalf("CreateConversation withMsgs: %v", err)
	}
	base := time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)
	hydrateAndSave(t, store, tenant, withMsgs.ID, inbox.MessageDirectionIn, "primeira mensagem do contato", base)
	hydrateAndSave(t, store, tenant, withMsgs.ID, inbox.MessageDirectionOut, "resposta do atendente", base.Add(time.Minute))

	empty, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), empty); err != nil {
		t.Fatalf("CreateConversation empty: %v", err)
	}

	got, err := store.ListConversationSummaries(context.Background(), tenant, inbox.ConversationFilter{}, 10)
	if err != nil {
		t.Fatalf("ListConversationSummaries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	byID := map[uuid.UUID]inbox.ConversationListItem{}
	for _, it := range got {
		byID[it.ID] = it
	}
	wm := byID[withMsgs.ID]
	if wm.LastMessageSnippet != "resposta do atendente" {
		t.Errorf("snippet = %q, want last (outbound) message body", wm.LastMessageSnippet)
	}
	if wm.LastMessageDirection != inbox.MessageDirectionOut {
		t.Errorf("direction = %q, want out", wm.LastMessageDirection)
	}
	// seedInboxContact sets display_name "Alice"; it wins over the channel
	// identifier fallback.
	if wm.ContactDisplayName != "Alice" {
		t.Errorf("ContactDisplayName = %q, want Alice", wm.ContactDisplayName)
	}
	em := byID[empty.ID]
	if em.LastMessageSnippet != "" || em.LastMessageDirection != "" {
		t.Errorf("empty conversation should carry no snippet/direction, got %q/%q", em.LastMessageSnippet, em.LastMessageDirection)
	}
}

func TestInboxAdapter_ListConversationSummaries_ContactNameFallback(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)

	// Contact with an empty display_name but a channel identity: the label
	// must fall back to the channel external_id, never blank.
	var contactID uuid.UUID
	if err := db.AdminPool().QueryRow(context.Background(),
		`INSERT INTO contact (tenant_id, display_name) VALUES ($1, '') RETURNING id`, tenant).Scan(&contactID); err != nil {
		t.Fatalf("seed nameless contact: %v", err)
	}
	if _, err := db.AdminPool().Exec(context.Background(),
		`INSERT INTO contact_channel_identity (tenant_id, contact_id, channel, external_id)
		 VALUES ($1, $2, 'whatsapp', '+5511888887777')`, tenant, contactID); err != nil {
		t.Fatalf("seed channel identity: %v", err)
	}
	conv := inbox.HydrateConversation(uuid.New(), tenant, contactID, "whatsapp",
		inbox.ConversationStateOpen, nil, time.Time{}, time.Time{})
	if err := store.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	got, err := store.ListConversationSummaries(context.Background(), tenant, inbox.ConversationFilter{}, 10)
	if err != nil {
		t.Fatalf("ListConversationSummaries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].ContactDisplayName != "+5511888887777" {
		t.Errorf("ContactDisplayName = %q, want channel external_id fallback", got[0].ContactDisplayName)
	}
}

func TestInboxAdapter_ListConversationSummaries_ChannelFilter(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)

	wa, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), wa); err != nil {
		t.Fatalf("CreateConversation wa: %v", err)
	}
	ig, _ := inbox.NewConversation(tenant, contact.ID, "instagram")
	if err := store.CreateConversation(context.Background(), ig); err != nil {
		t.Fatalf("CreateConversation ig: %v", err)
	}

	got, err := store.ListConversationSummaries(context.Background(), tenant, inbox.ConversationFilter{Channel: "instagram"}, 10)
	if err != nil {
		t.Fatalf("ListConversationSummaries: %v", err)
	}
	if len(got) != 1 || got[0].ID != ig.ID {
		t.Fatalf("channel filter = %+v, want only instagram conversation", got)
	}
}

func TestInboxAdapter_ListConversationSummaries_AssignedFilter(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)
	userA := seedUserForAssignment(t, db.AdminPool(), tenant)

	mine, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), mine); err != nil {
		t.Fatalf("CreateConversation mine: %v", err)
	}
	other, _ := inbox.NewConversation(tenant, contact.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), other); err != nil {
		t.Fatalf("CreateConversation other: %v", err)
	}
	if _, err := db.AdminPool().Exec(context.Background(),
		`UPDATE conversation SET assigned_user_id = $1 WHERE id = $2`, userA, mine.ID); err != nil {
		t.Fatalf("assign: %v", err)
	}

	got, err := store.ListConversationSummaries(context.Background(), tenant, inbox.ConversationFilter{AssignedUserID: userA}, 10)
	if err != nil {
		t.Fatalf("ListConversationSummaries: %v", err)
	}
	if len(got) != 1 || got[0].ID != mine.ID {
		t.Fatalf("assigned filter = %+v, want only the conversation assigned to userA", got)
	}
	if got[0].AssignedUserID == nil || *got[0].AssignedUserID != userA {
		t.Errorf("AssignedUserID = %v, want %s", got[0].AssignedUserID, userA)
	}
}

func TestInboxAdapter_ListConversationSummaries_TenantIsolation(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	contactA := seedInboxContact(t, db, tenantA)

	convA, _ := inbox.NewConversation(tenantA, contactA.ID, "whatsapp")
	if err := store.CreateConversation(context.Background(), convA); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	got, err := store.ListConversationSummaries(context.Background(), tenantB, inbox.ConversationFilter{}, 10)
	if err != nil {
		t.Fatalf("ListConversationSummaries B: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("tenant B saw %d rows, want 0 (RLS isolation)", len(got))
	}
}

func TestInboxAdapter_ListConversationSummaries_RejectsBadInput(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	if _, err := store.ListConversationSummaries(context.Background(), uuid.Nil, inbox.ConversationFilter{}, 10); err == nil {
		t.Error("zero tenant err = nil")
	}
	tenant := seedContactsTenant(t, db)
	if _, err := store.ListConversationSummaries(context.Background(), tenant, inbox.ConversationFilter{}, 0); err == nil {
		t.Error("limit=0 err = nil")
	}
	if _, err := store.ListConversationSummaries(context.Background(), tenant, inbox.ConversationFilter{}, 999999); err != nil {
		t.Errorf("clamp err = %v", err)
	}
}

func newUserDirectory(t *testing.T, db *testpg.DB) *pginbox.UserDirectory {
	t.Helper()
	d, err := pginbox.NewUserDirectory(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewUserDirectory: %v", err)
	}
	return d
}

func TestInboxUserDirectory_New_RejectsNilPool(t *testing.T) {
	if _, err := pginbox.NewUserDirectory(nil); err == nil {
		t.Error("NewUserDirectory(nil) err = nil")
	}
}

func TestInboxUserDirectory_LabelsByID(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	dir := newUserDirectory(t, db)
	tenant := seedContactsTenant(t, db)
	userA := seedUserForAssignment(t, db.AdminPool(), tenant)
	userB := seedUserForAssignment(t, db.AdminPool(), tenant)
	missing := uuid.New()

	got, err := dir.LabelsByID(context.Background(), tenant, []uuid.UUID{userA, userB, missing})
	if err != nil {
		t.Fatalf("LabelsByID: %v", err)
	}
	// seedUserForAssignment uses "<uuid>@test" → local part is the uuid.
	if got[userA] != userA.String() {
		t.Errorf("label[A] = %q, want %q", got[userA], userA.String())
	}
	if got[userB] != userB.String() {
		t.Errorf("label[B] = %q, want %q", got[userB], userB.String())
	}
	if _, ok := got[missing]; ok {
		t.Errorf("missing id should be absent, got %q", got[missing])
	}
}

func TestInboxUserDirectory_EmptyIDs(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	dir := newUserDirectory(t, db)
	tenant := seedContactsTenant(t, db)
	got, err := dir.LabelsByID(context.Background(), tenant, nil)
	if err != nil {
		t.Fatalf("LabelsByID: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty ids should yield empty map, got %v", got)
	}
}

func TestInboxUserDirectory_RejectsNilTenant(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	dir := newUserDirectory(t, db)
	if _, err := dir.LabelsByID(context.Background(), uuid.Nil, []uuid.UUID{uuid.New()}); err == nil {
		t.Error("nil tenant err = nil")
	}
}

func TestInboxUserDirectory_TenantIsolation(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	dir := newUserDirectory(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	userA := seedUserForAssignment(t, db.AdminPool(), tenantA)

	// Looking up tenant A's user under tenant B must return nothing — RLS
	// on users hides cross-tenant rows even with a caller-supplied id.
	got, err := dir.LabelsByID(context.Background(), tenantB, []uuid.UUID{userA})
	if err != nil {
		t.Fatalf("LabelsByID: %v", err)
	}
	if _, ok := got[userA]; ok {
		t.Errorf("tenant B resolved tenant A's user: %q", got[userA])
	}
}

// insertTenantChannel inserts a tenant_channels row (migration 0128)
// under the admin pool and returns its id, so conversations can be routed
// to a real channel instance (the conversation.channel_id FK requires it).
func insertTenantChannel(t *testing.T, db *testpg.DB, tenantID uuid.UUID, channelKey, externalID string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.AdminPool().QueryRow(context.Background(),
		`INSERT INTO tenant_channels (tenant_id, channel_key, external_id, display_name)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		tenantID, channelKey, externalID, channelKey).Scan(&id); err != nil {
		t.Fatalf("insert tenant_channel: %v", err)
	}
	return id
}

// TestInboxAdapter_ListConversationSummaries_ChannelScope is the SIN-66378
// P4 per-channel access filter on the live read path. It proves:
//   - a nil ChannelScope lists every conversation (gerente / legacy);
//   - a non-nil scope restricts to conversations whose channel_id is in
//     the set, and excludes both out-of-scope and NULL channel_id rows
//     (deny-by-default);
//   - an empty (non-nil) scope yields nothing;
//   - the ChannelID chip narrows to a single instance and AND-s with the
//     scope so an out-of-scope chip value leaks nothing;
//   - conversation.channel_id round-trips through CreateConversation.
func TestInboxAdapter_ListConversationSummaries_ChannelScope(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newInboxStore(t, db)
	ctx := context.Background()
	tenant := seedContactsTenant(t, db)
	contact := seedInboxContact(t, db, tenant)

	chA := insertTenantChannel(t, db, tenant, "whatsapp", "+5511111110000")
	chB := insertTenantChannel(t, db, tenant, "whatsapp", "+5511222220000")

	// Two conversations on A, one on B, one unrouted (nil channel_id).
	mk := func(chID *uuid.UUID) uuid.UUID {
		c, err := inbox.NewConversation(tenant, contact.ID, "whatsapp")
		if err != nil {
			t.Fatalf("NewConversation: %v", err)
		}
		if chID != nil {
			c.RouteToChannel(*chID)
		}
		if err := store.CreateConversation(ctx, c); err != nil {
			t.Fatalf("CreateConversation: %v", err)
		}
		return c.ID
	}
	a1 := mk(&chA)
	a2 := mk(&chA)
	b1 := mk(&chB)
	_ = mk(nil) // unrouted

	ids := func(items []inbox.ConversationListItem) map[uuid.UUID]bool {
		m := map[uuid.UUID]bool{}
		for _, it := range items {
			m[it.ID] = true
		}
		return m
	}

	// nil scope → everything (all 4).
	all, err := store.ListConversationSummaries(ctx, tenant, inbox.ConversationFilter{}, 50)
	if err != nil {
		t.Fatalf("nil scope: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("nil scope count = %d, want 4", len(all))
	}

	// scope {A} → only a1, a2; not b1, not the unrouted one.
	scopeA := []uuid.UUID{chA}
	gotA, err := store.ListConversationSummaries(ctx, tenant, inbox.ConversationFilter{ChannelScope: &scopeA}, 50)
	if err != nil {
		t.Fatalf("scope A: %v", err)
	}
	setA := ids(gotA)
	if len(setA) != 2 || !setA[a1] || !setA[a2] {
		t.Errorf("scope {A} = %v, want {a1,a2}", setA)
	}
	if setA[b1] {
		t.Error("scope {A} leaked a channel-B conversation")
	}

	// empty (non-nil) scope → nothing (deny-by-default).
	empty := []uuid.UUID{}
	gotEmpty, err := store.ListConversationSummaries(ctx, tenant, inbox.ConversationFilter{ChannelScope: &empty}, 50)
	if err != nil {
		t.Fatalf("empty scope: %v", err)
	}
	if len(gotEmpty) != 0 {
		t.Errorf("empty scope count = %d, want 0", len(gotEmpty))
	}

	// ChannelID chip narrows to B alone.
	chipB := chB
	gotChip, err := store.ListConversationSummaries(ctx, tenant, inbox.ConversationFilter{ChannelID: &chipB}, 50)
	if err != nil {
		t.Fatalf("chip B: %v", err)
	}
	if len(gotChip) != 1 || gotChip[0].ID != b1 {
		t.Errorf("chip B = %v, want only b1", ids(gotChip))
	}

	// scope {A} AND chip B → intersection empty (no leak).
	gotCross, err := store.ListConversationSummaries(ctx, tenant, inbox.ConversationFilter{ChannelScope: &scopeA, ChannelID: &chipB}, 50)
	if err != nil {
		t.Fatalf("scope A + chip B: %v", err)
	}
	if len(gotCross) != 0 {
		t.Errorf("scope {A} + chip B count = %d, want 0 (out-of-scope chip)", len(gotCross))
	}
}
