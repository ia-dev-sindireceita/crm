package contacts_test

// SIN-62726 integration tests for the contacts Postgres adapter.
//
// Every test spins up a fresh database via the shared testpg harness
// (TestMain below), applies the migration chain the adapter needs
// (0004 tenants → 0005 users → 0088 inbox/contacts) on top of the
// harness's default 0001-0003, then drives the adapter end-to-end
// against the real cluster.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	contactsdomain "github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
)

var harness *testpg.Harness

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	h, err := testpg.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testpg.Start: %v\n", err)
		os.Exit(2)
	}
	harness = h
	code := m.Run()
	if err := h.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "testpg.Stop: %v\n", err)
	}
	os.Exit(code)
}

// freshDB applies the migration chain the contacts adapter needs.
func freshDB(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
	} {
		body, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	return db
}

// seedTenant inserts a tenant row (the contact table FKs to tenants(id))
// and returns its id.
func seedTenant(t *testing.T, db *testpg.DB) uuid.UUID {
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

func newStore(t *testing.T, db *testpg.DB) *contacts.Store {
	t.Helper()
	s, err := contacts.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("contacts.New: %v", err)
	}
	return s
}

func TestNew_RejectsNilPool(t *testing.T) {
	if _, err := contacts.New(nil); err == nil {
		t.Error("New(nil) err = nil, want postgres.ErrNilPool")
	}
}

func TestSave_RejectsNilContact(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	if err := store.Save(context.Background(), nil); err == nil {
		t.Error("Save(nil) err = nil, want error")
	}
}

func TestSave_RejectsZeroFields(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	zeroTenant := contactsdomain.Hydrate(uuid.New(), uuid.Nil, "A", nil, time.Now().UTC(), time.Now().UTC())
	if err := store.Save(context.Background(), zeroTenant); err == nil {
		t.Error("Save(zero tenant) err = nil")
	}
	zeroID := contactsdomain.Hydrate(uuid.Nil, uuid.New(), "A", nil, time.Now().UTC(), time.Now().UTC())
	if err := store.Save(context.Background(), zeroID); err == nil {
		t.Error("Save(zero id) err = nil")
	}
}

func TestSave_RoundTrip(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	tenant := seedTenant(t, db)

	c, err := contactsdomain.New(tenant, "Alice")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.AddChannelIdentity(contactsdomain.ChannelWhatsApp, "+5511999990001"); err != nil {
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

func TestSave_UsesClockForZeroTimestamps(t *testing.T) {
	db := freshDB(t)
	tenant := seedTenant(t, db)
	pinned := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	store := newStore(t, db).WithClock(func() time.Time { return pinned })

	c := contactsdomain.Hydrate(uuid.New(), tenant, "Bob",
		[]contactsdomain.ChannelIdentity{{Channel: "email", ExternalID: "bob@example.com"}},
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

func TestFindByID_NotFound(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	tenant := seedTenant(t, db)
	_, err := store.FindByID(context.Background(), tenant, uuid.New())
	if !errors.Is(err, contactsdomain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFindByID_RejectsZeroTenant(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	if _, err := store.FindByID(context.Background(), uuid.Nil, uuid.New()); err == nil {
		t.Error("FindByID(nil tenant) err = nil")
	}
}

func TestFindByID_NilContactID_NotFound(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	tenant := seedTenant(t, db)
	_, err := store.FindByID(context.Background(), tenant, uuid.Nil)
	if !errors.Is(err, contactsdomain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFindByID_CrossTenantHiddenByRLS(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	tenantA := seedTenant(t, db)
	tenantB := seedTenant(t, db)

	c, _ := contactsdomain.New(tenantA, "Alice")
	if err := c.AddChannelIdentity("email", "alice@example.com"); err != nil {
		t.Fatalf("AddChannelIdentity: %v", err)
	}
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Tenant B asks for tenant A's id: RLS hides the row → ErrNotFound.
	_, err := store.FindByID(context.Background(), tenantB, c.ID)
	if !errors.Is(err, contactsdomain.ErrNotFound) {
		t.Errorf("cross-tenant err = %v, want ErrNotFound", err)
	}
}

func TestFindByChannelIdentity_HappyPath(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	tenant := seedTenant(t, db)

	c, _ := contactsdomain.New(tenant, "Alice")
	if err := c.AddChannelIdentity(contactsdomain.ChannelWhatsApp, "+5511999990001"); err != nil {
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

func TestFindByChannelIdentity_NotFound(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	tenant := seedTenant(t, db)
	_, err := store.FindByChannelIdentity(context.Background(), tenant, "whatsapp", "+5511999990001")
	if !errors.Is(err, contactsdomain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFindByChannelIdentity_NormalisesChannelAndTrims(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	tenant := seedTenant(t, db)

	c, _ := contactsdomain.New(tenant, "Alice")
	if err := c.AddChannelIdentity(contactsdomain.ChannelWhatsApp, "+5511999990001"); err != nil {
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

func TestFindByChannelIdentity_InvalidShape_NotFound(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	tenant := seedTenant(t, db)
	_, err := store.FindByChannelIdentity(context.Background(), tenant, "whatsapp", "not-e164")
	if !errors.Is(err, contactsdomain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFindByChannelIdentity_RejectsZeroTenant(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	if _, err := store.FindByChannelIdentity(context.Background(), uuid.Nil, "whatsapp", "+5511999990001"); err == nil {
		t.Error("FindByChannelIdentity(nil tenant) err = nil")
	}
}

func TestFindByChannelIdentity_CrossTenant_HiddenByRLS(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	tenantA := seedTenant(t, db)
	tenantB := seedTenant(t, db)

	c, _ := contactsdomain.New(tenantA, "Alice")
	if err := c.AddChannelIdentity(contactsdomain.ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("AddChannelIdentity: %v", err)
	}
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err := store.FindByChannelIdentity(context.Background(), tenantB, "whatsapp", "+5511999990001")
	if !errors.Is(err, contactsdomain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSave_DuplicateChannelExternal_ReturnsConflict(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	tenantA := seedTenant(t, db)
	tenantB := seedTenant(t, db)

	first, _ := contactsdomain.New(tenantA, "Alice")
	if err := first.AddChannelIdentity(contactsdomain.ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("first AddChannelIdentity: %v", err)
	}
	if err := store.Save(context.Background(), first); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	second, _ := contactsdomain.New(tenantB, "Bob")
	if err := second.AddChannelIdentity(contactsdomain.ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("second AddChannelIdentity: %v", err)
	}
	err := store.Save(context.Background(), second)
	if !errors.Is(err, contactsdomain.ErrChannelIdentityConflict) {
		t.Errorf("err = %v, want ErrChannelIdentityConflict", err)
	}

	// Confirm rollback: tenant B's contact row was NOT persisted.
	_, ferr := store.FindByID(context.Background(), tenantB, second.ID)
	if !errors.Is(ferr, contactsdomain.ErrNotFound) {
		t.Errorf("after conflict, tenant B's contact persisted; err = %v", ferr)
	}
}

func TestSave_SecondCallSameContactID_IsError(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	tenant := seedTenant(t, db)

	c, _ := contactsdomain.New(tenant, "Alice")
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

// TestUpsertContactByChannel_HighConcurrency is AC #4: 100 concurrent
// callers with the same (tenant, channel, external_id) must result in
// exactly one contact row and 99 calls returning the existing one.
// Driving through the use-case + real Postgres exercises the full
// idempotency contract end-to-end.
func TestUpsertContactByChannel_HighConcurrency(t *testing.T) {
	db := freshDB(t)
	tenant := seedTenant(t, db)
	store := newStore(t, db)
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

// TestSave_HydratedContactPreservesTimestamps verifies that explicit
// CreatedAt/UpdatedAt on a hydrated contact are persisted as-is, which
// matters when the adapter is used downstream of replay/import code.
func TestSave_HydratedContactPreservesTimestamps(t *testing.T) {
	db := freshDB(t)
	store := newStore(t, db)
	tenant := seedTenant(t, db)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	c := contactsdomain.Hydrate(uuid.New(), tenant, "Alice",
		[]contactsdomain.ChannelIdentity{{Channel: "email", ExternalID: "alice@example.com"}},
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
