package postgres_test

// SIN-62726 integration tests for the contacts Postgres adapter.
//
// These live in the parent postgres_test package (not the
// internal/adapter/db/postgres/contacts subpackage) so they share the
// TestMain / harness with withtenant_test.go,
// inbox_contacts_migration_test.go, account_lockout_test.go, etc.
//
// Why parent package? `go test -race ./...` starts every package's test
// binary in parallel. Each binary that calls testpg.Start() bootstraps
// the SHARED Postgres cluster (CI TEST_DATABASE_URL) by ALTERing the
// app_admin / app_runtime / app_master_ops role passwords to its own
// per-process value. Two binaries racing on that ALTER yield SQLSTATE
// 28P01 (password authentication failed) for whichever bootstrap was
// overwritten — the deterministic failure pattern observed on the
// initial PR #79 CI run. The existing mastersession adapter dodged
// this by keeping its code in a subpackage (db/postgres/mastersession)
// but its tests in the parent package (db/postgres/mastersession_test.go).
// We follow that pattern.
//
// freshDBWithInboxContacts and seedTenantUserMaster are reused from
// inbox_contacts_migration_test.go and audit_helpers_test.go.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
)

// seedContactsTenant inserts a fresh tenant suitable for contact-FK
// tests and returns its id. seedTenantUserMaster also creates a master
// user, which the contacts tests do not need; this is a lighter
// per-test seed.
func seedContactsTenant(t *testing.T, db *testpg.DB) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, fmt.Sprintf("t-%s", id), fmt.Sprintf("%s.crm.local", id)); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func newContactsStore(t *testing.T, db *testpg.DB) *pgcontacts.Store {
	t.Helper()
	s, err := pgcontacts.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgcontacts.New: %v", err)
	}
	return s
}

func TestContactsAdapter_New_RejectsNilPool(t *testing.T) {
	if _, err := pgcontacts.New(nil); err == nil {
		t.Error("New(nil) err = nil, want postgres.ErrNilPool")
	}
}

func TestContactsAdapter_Save_RejectsNilContact(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	if err := store.Save(context.Background(), nil); err == nil {
		t.Error("Save(nil) err = nil, want error")
	}
}

func TestContactsAdapter_Save_RejectsZeroFields(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	zeroTenant := contacts.Hydrate(uuid.New(), uuid.Nil, "A", nil, time.Now().UTC(), time.Now().UTC())
	if err := store.Save(context.Background(), zeroTenant); err == nil {
		t.Error("Save(zero tenant) err = nil")
	}
	zeroID := contacts.Hydrate(uuid.Nil, uuid.New(), "A", nil, time.Now().UTC(), time.Now().UTC())
	if err := store.Save(context.Background(), zeroID); err == nil {
		t.Error("Save(zero id) err = nil")
	}
}

func TestContactsAdapter_Save_RoundTrip(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)

	c, err := contacts.New(tenant, "Alice")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.AddChannelIdentity(contacts.ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("AddChannelIdentity: %v", err)
	}
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.FindByID(context.Background(), tenant, c.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.DisplayName != "Alice" {
		t.Errorf("DisplayName = %q, want Alice", got.DisplayName)
	}
	if got.TenantID != tenant {
		t.Errorf("TenantID = %s, want %s", got.TenantID, tenant)
	}
	if len(got.Identities()) != 1 || got.Identities()[0].ExternalID != "+5511999990001" {
		t.Errorf("identities = %+v", got.Identities())
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not persisted: %+v / %+v", got.CreatedAt, got.UpdatedAt)
	}
}

func TestContactsAdapter_Save_UsesClockForZeroTimestamps(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	tenant := seedContactsTenant(t, db)
	pinned := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	store := newContactsStore(t, db).WithClock(func() time.Time { return pinned })

	c := contacts.Hydrate(uuid.New(), tenant, "Bob",
		[]contacts.ChannelIdentity{{Channel: "email", ExternalID: "bob@example.com"}},
		time.Time{}, time.Time{},
	)
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.FindByID(context.Background(), tenant, c.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if !got.CreatedAt.Equal(pinned) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, pinned)
	}
	if !got.UpdatedAt.Equal(pinned) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, pinned)
	}
}

func TestContactsAdapter_FindByID_NotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)
	_, err := store.FindByID(context.Background(), tenant, uuid.New())
	if !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestContactsAdapter_FindByID_RejectsZeroTenant(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	if _, err := store.FindByID(context.Background(), uuid.Nil, uuid.New()); err == nil {
		t.Error("FindByID(nil tenant) err = nil")
	}
}

func TestContactsAdapter_FindByID_NilContactID_NotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)
	_, err := store.FindByID(context.Background(), tenant, uuid.Nil)
	if !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestContactsAdapter_FindByID_CrossTenantHiddenByRLS(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)

	c, _ := contacts.New(tenantA, "Alice")
	if err := c.AddChannelIdentity("email", "alice@example.com"); err != nil {
		t.Fatalf("AddChannelIdentity: %v", err)
	}
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Tenant B asks for tenant A's id: RLS hides the row → ErrNotFound.
	_, err := store.FindByID(context.Background(), tenantB, c.ID)
	if !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("cross-tenant err = %v, want ErrNotFound", err)
	}
}

func TestContactsAdapter_FindByChannelIdentity_HappyPath(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)

	c, _ := contacts.New(tenant, "Alice")
	if err := c.AddChannelIdentity(contacts.ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("AddChannelIdentity: %v", err)
	}
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.FindByChannelIdentity(context.Background(), tenant, "whatsapp", "+5511999990001")
	if err != nil {
		t.Fatalf("FindByChannelIdentity: %v", err)
	}
	if got.ID != c.ID {
		t.Errorf("ID = %s, want %s", got.ID, c.ID)
	}
}

func TestContactsAdapter_FindByChannelIdentity_NotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)
	_, err := store.FindByChannelIdentity(context.Background(), tenant, "whatsapp", "+5511999990001")
	if !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestContactsAdapter_FindByChannelIdentity_NormalisesChannelAndTrims(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)

	c, _ := contacts.New(tenant, "Alice")
	if err := c.AddChannelIdentity(contacts.ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("AddChannelIdentity: %v", err)
	}
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.FindByChannelIdentity(context.Background(), tenant, " WhatsApp ", " +5511999990001 ")
	if err != nil {
		t.Fatalf("normalised find: %v", err)
	}
	if got.ID != c.ID {
		t.Errorf("ID = %s, want %s", got.ID, c.ID)
	}
}

func TestContactsAdapter_FindByChannelIdentity_InvalidShape_NotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)
	_, err := store.FindByChannelIdentity(context.Background(), tenant, "whatsapp", "not-e164")
	if !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestContactsAdapter_FindByChannelIdentity_RejectsZeroTenant(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	if _, err := store.FindByChannelIdentity(context.Background(), uuid.Nil, "whatsapp", "+5511999990001"); err == nil {
		t.Error("FindByChannelIdentity(nil tenant) err = nil")
	}
}

func TestContactsAdapter_FindByChannelIdentity_CrossTenant_HiddenByRLS(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)

	c, _ := contacts.New(tenantA, "Alice")
	if err := c.AddChannelIdentity(contacts.ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("AddChannelIdentity: %v", err)
	}
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err := store.FindByChannelIdentity(context.Background(), tenantB, "whatsapp", "+5511999990001")
	if !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestContactsAdapter_Save_DuplicateChannelExternal_ReturnsConflict(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)

	first, _ := contacts.New(tenantA, "Alice")
	if err := first.AddChannelIdentity(contacts.ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("first AddChannelIdentity: %v", err)
	}
	if err := store.Save(context.Background(), first); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	second, _ := contacts.New(tenantB, "Bob")
	if err := second.AddChannelIdentity(contacts.ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("second AddChannelIdentity: %v", err)
	}
	err := store.Save(context.Background(), second)
	if !errors.Is(err, contacts.ErrChannelIdentityConflict) {
		t.Errorf("err = %v, want ErrChannelIdentityConflict", err)
	}

	// Confirm rollback: tenant B's contact row was NOT persisted.
	_, ferr := store.FindByID(context.Background(), tenantB, second.ID)
	if !errors.Is(ferr, contacts.ErrNotFound) {
		t.Errorf("after conflict, tenant B's contact persisted; err = %v", ferr)
	}
}

func TestContactsAdapter_Save_SecondCallSameContactID_IsError(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)

	c, _ := contacts.New(tenant, "Alice")
	if err := c.AddChannelIdentity("email", "alice@example.com"); err != nil {
		t.Fatalf("AddChannelIdentity: %v", err)
	}
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := store.Save(context.Background(), c); err == nil {
		t.Error("second Save(same contact) err = nil, want PK-violation error")
	}
}

// TestContactsAdapter_UpsertContactByChannel_HighConcurrency is AC #4:
// 100 concurrent callers with the same (tenant, channel, external_id)
// must result in exactly one contact row and 99 calls returning the
// existing one. Driving through the use-case + real Postgres exercises
// the full idempotency contract end-to-end.
func TestContactsAdapter_UpsertContactByChannel_HighConcurrency(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	tenant := seedContactsTenant(t, db)
	store := newContactsStore(t, db)
	u := contactsusecase.MustNew(store)

	const n = 100
	var created atomic.Int64
	type outcome struct {
		id uuid.UUID
	}
	results := make(chan outcome, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := u.Execute(ctx, contactsusecase.Input{
				TenantID:    tenant,
				Channel:     "whatsapp",
				ExternalID:  "+5511999990001",
				DisplayName: fmt.Sprintf("caller-%d", i),
			})
			if err != nil {
				errs <- err
				return
			}
			if res.Created {
				created.Add(1)
			}
			results <- outcome{id: res.Contact.ID}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	if e := <-errs; e != nil {
		t.Fatalf("concurrent Execute failed: %v", e)
	}
	if got := created.Load(); got != 1 {
		t.Errorf("Created=true count = %d, want 1", got)
	}

	var firstID uuid.UUID
	count := 0
	for r := range results {
		count++
		if firstID == uuid.Nil {
			firstID = r.id
		} else if r.id != firstID {
			t.Errorf("contact id mismatch: %s vs %s", r.id, firstID)
		}
	}
	if count != n {
		t.Errorf("got %d results, want %d", count, n)
	}

	// Confirm exactly one row at the DB level.
	var dbCount int
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT count(*) FROM contact WHERE tenant_id = $1`, tenant).Scan(&dbCount); err != nil {
		t.Fatalf("count contacts: %v", err)
	}
	if dbCount != 1 {
		t.Errorf("contact rows = %d, want 1", dbCount)
	}
	var idCount int
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT count(*) FROM contact_channel_identity WHERE tenant_id = $1`, tenant).Scan(&idCount); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if idCount != 1 {
		t.Errorf("identity rows = %d, want 1", idCount)
	}
}

func TestContactsAdapter_Save_HydratedContactPreservesTimestamps(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	c := contacts.Hydrate(uuid.New(), tenant, "Alice",
		[]contacts.ChannelIdentity{{Channel: "email", ExternalID: "alice@example.com"}},
		t0, t1,
	)
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.FindByID(context.Background(), tenant, c.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if !got.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, t0)
	}
	if !got.UpdatedAt.Equal(t1) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, t1)
	}
}
