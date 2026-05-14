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

// TestInboxAdapter_ReceiveInbound_ConcurrentIdempotent races 50
// callers on the same dedup key against the real Postgres UNIQUE
// constraint. Exactly one message + one contact must end up
// persisted; the other 49 callers report Duplicate.
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
	const n = 50
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
