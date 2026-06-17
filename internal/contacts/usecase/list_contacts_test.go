package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
)

// seedContact builds a contact with the given name + optional identities
// and stores it in the fake repo. Fails the test on any construction error.
func seedContact(t *testing.T, repo *fakeRepo, tenant uuid.UUID, name string, ids ...contacts.ChannelIdentity) *contacts.Contact {
	t.Helper()
	c, err := contacts.New(tenant, name)
	if err != nil {
		t.Fatalf("seed New(%q): %v", name, err)
	}
	for _, id := range ids {
		if err := c.AddChannelIdentity(id.Channel, id.ExternalID); err != nil {
			t.Fatalf("seed AddChannelIdentity(%+v): %v", id, err)
		}
	}
	if err := repo.Save(context.Background(), c); err != nil {
		t.Fatalf("seed Save(%q): %v", name, err)
	}
	return c
}

func TestNewListContacts_RejectsNilRepo(t *testing.T) {
	if u, err := NewListContacts(nil); err == nil || u != nil {
		t.Errorf("NewListContacts(nil) = (%v, %v), want (nil, error)", u, err)
	}
}

func TestMustNewListContacts_PanicsOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustNewListContacts(nil) did not panic")
		}
	}()
	_ = MustNewListContacts(nil)
}

func TestListContacts_RejectsNilTenant(t *testing.T) {
	u := MustNewListContacts(newFakeRepo())
	_, err := u.Execute(context.Background(), ListContactsInput{TenantID: uuid.Nil})
	if !errors.Is(err, contacts.ErrInvalidTenant) {
		t.Errorf("err = %v, want ErrInvalidTenant", err)
	}
}

func TestListContacts_EmptyResult(t *testing.T) {
	u := MustNewListContacts(newFakeRepo())
	res, err := u.Execute(context.Background(), ListContactsInput{TenantID: uuid.New()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Total != 0 || len(res.Items) != 0 {
		t.Errorf("expected empty result, got total=%d items=%d", res.Total, len(res.Items))
	}
	if res.Limit != contacts.DefaultListLimit {
		t.Errorf("Limit = %d, want default %d", res.Limit, contacts.DefaultListLimit)
	}
}

func TestListContacts_ReturnsTenantScopedAndOrdered(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	other := uuid.New()
	seedContact(t, repo, tenant, "Charlie")
	seedContact(t, repo, tenant, "Alice")
	seedContact(t, repo, tenant, "Bob")
	seedContact(t, repo, other, "Zoe") // different tenant: must not appear

	u := MustNewListContacts(repo)
	res, err := u.Execute(context.Background(), ListContactsInput{TenantID: tenant})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Total != 3 {
		t.Errorf("Total = %d, want 3", res.Total)
	}
	gotNames := make([]string, len(res.Items))
	for i, it := range res.Items {
		gotNames[i] = it.DisplayName
	}
	want := []string{"Alice", "Bob", "Charlie"}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Errorf("ordered names = %v, want %v", gotNames, want)
			break
		}
	}
}

func TestListContacts_SearchByNameAndIdentity(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	seedContact(t, repo, tenant, "Alice", contacts.ChannelIdentity{Channel: "whatsapp", ExternalID: "+5511999990001"})
	seedContact(t, repo, tenant, "Bob", contacts.ChannelIdentity{Channel: "email", ExternalID: "bob@example.com"})

	u := MustNewListContacts(repo)

	// Match by name.
	res, err := u.Execute(context.Background(), ListContactsInput{TenantID: tenant, Query: "ali"})
	if err != nil {
		t.Fatalf("name search: %v", err)
	}
	if res.Total != 1 || res.Items[0].DisplayName != "Alice" {
		t.Errorf("name search got total=%d items=%+v", res.Total, res.Items)
	}

	// Match by identity external id (email).
	res, err = u.Execute(context.Background(), ListContactsInput{TenantID: tenant, Query: "bob@example"})
	if err != nil {
		t.Fatalf("identity search: %v", err)
	}
	if res.Total != 1 || res.Items[0].DisplayName != "Bob" {
		t.Errorf("identity search got total=%d items=%+v", res.Total, res.Items)
	}

	// Match by phone fragment.
	res, err = u.Execute(context.Background(), ListContactsInput{TenantID: tenant, Query: "999990001"})
	if err != nil {
		t.Fatalf("phone search: %v", err)
	}
	if res.Total != 1 || res.Items[0].DisplayName != "Alice" {
		t.Errorf("phone search got total=%d items=%+v", res.Total, res.Items)
	}
}

func TestListContacts_Pagination(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	for _, n := range []string{"A", "B", "C", "D", "E"} {
		seedContact(t, repo, tenant, n)
	}
	u := MustNewListContacts(repo)

	res, err := u.Execute(context.Background(), ListContactsInput{TenantID: tenant, Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Total != 5 {
		t.Errorf("Total = %d, want 5 (total ignores pagination)", res.Total)
	}
	if len(res.Items) != 2 {
		t.Fatalf("page size = %d, want 2", len(res.Items))
	}
	if res.Items[0].DisplayName != "C" || res.Items[1].DisplayName != "D" {
		t.Errorf("page = %q,%q want C,D", res.Items[0].DisplayName, res.Items[1].DisplayName)
	}
	if res.Limit != 2 || res.Offset != 2 {
		t.Errorf("Limit/Offset = %d/%d, want 2/2", res.Limit, res.Offset)
	}
}

func TestListContacts_OffsetPastEnd_EmptyPageRealTotal(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	seedContact(t, repo, tenant, "A")
	u := MustNewListContacts(repo)
	res, err := u.Execute(context.Background(), ListContactsInput{TenantID: tenant, Limit: 10, Offset: 99})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Total != 1 {
		t.Errorf("Total = %d, want 1", res.Total)
	}
	if len(res.Items) != 0 {
		t.Errorf("items = %d, want 0", len(res.Items))
	}
}

func TestListContacts_ProjectsIdentitiesAndChannels(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	seedContact(t, repo, tenant, "Alice",
		contacts.ChannelIdentity{Channel: "whatsapp", ExternalID: "+5511999990001"},
		contacts.ChannelIdentity{Channel: "email", ExternalID: "alice@example.com"},
	)
	u := MustNewListContacts(repo)
	res, err := u.Execute(context.Background(), ListContactsInput{TenantID: tenant})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(res.Items))
	}
	it := res.Items[0]
	if len(it.Identities) != 2 {
		t.Errorf("identities = %d, want 2", len(it.Identities))
	}
	// Channels are sorted + de-duplicated.
	if len(it.Channels) != 2 || it.Channels[0] != "email" || it.Channels[1] != "whatsapp" {
		t.Errorf("Channels = %v, want [email whatsapp]", it.Channels)
	}
}

func TestListContacts_PropagatesRepoError(t *testing.T) {
	repo := newFakeRepo()
	sentinel := errors.New("synthetic list failure")
	repo.findErr = sentinel
	u := MustNewListContacts(repo)
	_, err := u.Execute(context.Background(), ListContactsInput{TenantID: uuid.New()})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}
