package postgres_test

// SIN-64976 integration tests for the contacts Postgres adapter's
// List + Update methods (the management surface added on top of the
// SIN-62726 Save/FindByID/FindByChannelIdentity port).
//
// Like contacts_adapter_test.go these live in the parent postgres_test
// package so they share the TestMain / harness and do not race the
// ALTER ROLE bootstrap on the shared CI cluster. Helpers
// (freshDBWithInboxContacts, seedContactsTenant, newContactsStore) are
// reused from inbox_contacts_migration_test.go / contacts_adapter_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/contacts"
)

// seedNamedContact creates and persists a contact with the given name and
// optional identities, returning the stored aggregate.
func seedNamedContact(t *testing.T, db *testpg.DB, tenant uuid.UUID, name string, ids ...contacts.ChannelIdentity) *contacts.Contact {
	t.Helper()
	store := newContactsStore(t, db)
	c, err := contacts.New(tenant, name)
	if err != nil {
		t.Fatalf("contacts.New(%q): %v", name, err)
	}
	for _, id := range ids {
		if err := c.AddChannelIdentity(id.Channel, id.ExternalID); err != nil {
			t.Fatalf("AddChannelIdentity(%+v): %v", id, err)
		}
	}
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatalf("Save(%q): %v", name, err)
	}
	return c
}

func TestContactsAdapter_List_RejectsZeroTenant(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	if _, _, err := store.List(context.Background(), uuid.Nil, contacts.ListFilter{}); err == nil {
		t.Error("List(nil tenant) err = nil, want error")
	}
}

func TestContactsAdapter_List_EmptyTenant(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)
	items, total, err := store.List(context.Background(), tenant, contacts.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Errorf("empty tenant: total=%d items=%d, want 0/0", total, len(items))
	}
}

func TestContactsAdapter_List_OrderedAndTotal(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)
	seedNamedContact(t, db, tenant, "Charlie")
	seedNamedContact(t, db, tenant, "Alice")
	seedNamedContact(t, db, tenant, "Bob")

	items, total, err := store.List(context.Background(), tenant, contacts.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	got := []string{items[0].DisplayName, items[1].DisplayName, items[2].DisplayName}
	want := []string{"Alice", "Bob", "Charlie"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ordered names = %v, want %v", got, want)
			break
		}
	}
}

func TestContactsAdapter_List_SearchByNameAndIdentity(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)
	seedNamedContact(t, db, tenant, "Alice", contacts.ChannelIdentity{Channel: "whatsapp", ExternalID: "+5511999990001"})
	seedNamedContact(t, db, tenant, "Bob", contacts.ChannelIdentity{Channel: "email", ExternalID: "bob@example.com"})

	cases := []struct {
		name      string
		query     string
		wantTotal int
		wantName  string
	}{
		{"by name case-insensitive", "ALI", 1, "Alice"},
		{"by phone fragment", "999990001", 1, "Alice"},
		{"by email fragment", "bob@example", 1, "Bob"},
		{"no match", "zzz", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, total, err := store.List(context.Background(), tenant, contacts.ListFilter{Query: tc.query})
			if err != nil {
				t.Fatalf("List(%q): %v", tc.query, err)
			}
			if total != tc.wantTotal {
				t.Fatalf("total = %d, want %d", total, tc.wantTotal)
			}
			if tc.wantName != "" && items[0].DisplayName != tc.wantName {
				t.Errorf("name = %q, want %q", items[0].DisplayName, tc.wantName)
			}
		})
	}
}

func TestContactsAdapter_List_Pagination(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)
	for _, n := range []string{"A", "B", "C", "D", "E"} {
		seedNamedContact(t, db, tenant, n)
	}
	items, total, err := store.List(context.Background(), tenant, contacts.ListFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(items) != 2 || items[0].DisplayName != "C" || items[1].DisplayName != "D" {
		t.Errorf("page = %+v, want C,D", names(items))
	}

	// Offset past the end → empty page, real total.
	items, total, err = store.List(context.Background(), tenant, contacts.ListFilter{Limit: 10, Offset: 99})
	if err != nil {
		t.Fatalf("List offset-past-end: %v", err)
	}
	if total != 5 || len(items) != 0 {
		t.Errorf("offset-past-end: total=%d items=%d, want 5/0", total, len(items))
	}
}

func TestContactsAdapter_List_TenantScopedByRLS(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	seedNamedContact(t, db, tenantA, "AliceA")
	seedNamedContact(t, db, tenantB, "BobB")

	items, total, err := store.List(context.Background(), tenantA, contacts.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].DisplayName != "AliceA" {
		t.Errorf("tenant A list = %v (total %d), want just AliceA", names(items), total)
	}
}

func TestContactsAdapter_List_LoadsIdentities(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)
	seedNamedContact(t, db, tenant, "Alice",
		contacts.ChannelIdentity{Channel: "whatsapp", ExternalID: "+5511999990001"},
		contacts.ChannelIdentity{Channel: "email", ExternalID: "alice@example.com"},
	)
	items, _, err := store.List(context.Background(), tenant, contacts.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if len(items[0].Identities()) != 2 {
		t.Errorf("identities = %d, want 2", len(items[0].Identities()))
	}
}

func TestContactsAdapter_List_EscapesLikeWildcards(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)
	seedNamedContact(t, db, tenant, "50% off coupon")
	seedNamedContact(t, db, tenant, "501 off road")

	// "50%" must match the literal-percent name only, not "501..." — the
	// adapter escapes % so it is not treated as a wildcard.
	items, total, err := store.List(context.Background(), tenant, contacts.ListFilter{Query: "50%"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].DisplayName != "50% off coupon" {
		t.Errorf("wildcard-escaped search = %v (total %d), want just '50%% off coupon'", names(items), total)
	}
}

func TestContactsAdapter_Update_RejectsNilAndZero(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	if err := store.Update(context.Background(), nil); err == nil {
		t.Error("Update(nil) err = nil")
	}
	zeroTenant := contacts.Hydrate(uuid.New(), uuid.Nil, "A", nil, time.Now().UTC(), time.Now().UTC())
	if err := store.Update(context.Background(), zeroTenant); err == nil {
		t.Error("Update(zero tenant) err = nil")
	}
	zeroID := contacts.Hydrate(uuid.Nil, uuid.New(), "A", nil, time.Now().UTC(), time.Now().UTC())
	if err := store.Update(context.Background(), zeroID); !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("Update(zero id) err = %v, want ErrNotFound", err)
	}
}

func TestContactsAdapter_Update_NotFound(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)
	ghost := contacts.Hydrate(uuid.New(), tenant, "Ghost", nil, time.Now().UTC(), time.Now().UTC())
	if err := store.Update(context.Background(), ghost); !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("Update(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestContactsAdapter_Update_PersistsNameAndTimestamp(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenant := seedContactsTenant(t, db)
	c := seedNamedContact(t, db, tenant, "Alice",
		contacts.ChannelIdentity{Channel: "whatsapp", ExternalID: "+5511999990001"})

	if err := c.Rename("Alicia"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := store.Update(context.Background(), c); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := store.FindByID(context.Background(), tenant, c.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.DisplayName != "Alicia" {
		t.Errorf("DisplayName = %q, want Alicia", got.DisplayName)
	}
	// Identities are untouched by Update.
	if len(got.Identities()) != 1 || got.Identities()[0].ExternalID != "+5511999990001" {
		t.Errorf("identities changed by Update: %+v", got.Identities())
	}
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Errorf("UpdatedAt %v before CreatedAt %v", got.UpdatedAt, got.CreatedAt)
	}
}

func TestContactsAdapter_Update_UsesClockForZeroTimestamp(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	tenant := seedContactsTenant(t, db)
	c := seedNamedContact(t, db, tenant, "Alice")

	pinned := time.Date(2026, 6, 16, 9, 30, 0, 0, time.UTC)
	store := newContactsStore(t, db).WithClock(func() time.Time { return pinned })
	// Hydrate a fresh aggregate with the same id but a zero UpdatedAt so
	// the adapter falls back to its clock.
	edit := contacts.Hydrate(c.ID, tenant, "Alicia", nil, c.CreatedAt, time.Time{})
	if err := store.Update(context.Background(), edit); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := store.FindByID(context.Background(), tenant, c.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if !got.UpdatedAt.Equal(pinned) {
		t.Errorf("UpdatedAt = %v, want clock-pinned %v", got.UpdatedAt, pinned)
	}
}

func TestContactsAdapter_Update_CrossTenantHiddenByRLS(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	store := newContactsStore(t, db)
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)
	c := seedNamedContact(t, db, tenantA, "Alice")

	// Tenant B forges the same id under its own tenant scope: RLS hides
	// tenant A's row → the UPDATE matches nothing → ErrNotFound.
	forged := contacts.Hydrate(c.ID, tenantB, "Hacked", nil, time.Now().UTC(), time.Now().UTC())
	if err := store.Update(context.Background(), forged); !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("cross-tenant Update err = %v, want ErrNotFound", err)
	}
	// Confirm tenant A's row is untouched.
	got, err := store.FindByID(context.Background(), tenantA, c.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.DisplayName != "Alice" {
		t.Errorf("tenant A name mutated to %q", got.DisplayName)
	}
}

// names is a tiny helper for readable failure messages.
func names(cs []*contacts.Contact) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.DisplayName
	}
	return out
}
