package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
)

func TestNewUpdateContact_RejectsNilRepo(t *testing.T) {
	if u, err := NewUpdateContact(nil); err == nil || u != nil {
		t.Errorf("NewUpdateContact(nil) = (%v, %v), want (nil, error)", u, err)
	}
}

func TestMustNewUpdateContact_PanicsOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustNewUpdateContact(nil) did not panic")
		}
	}()
	_ = MustNewUpdateContact(nil)
}

func TestUpdateContact_RejectsNilTenant(t *testing.T) {
	u := MustNewUpdateContact(newFakeRepo())
	_, err := u.Execute(context.Background(), UpdateContactInput{TenantID: uuid.Nil, ContactID: uuid.New(), DisplayName: "X"})
	if !errors.Is(err, contacts.ErrInvalidTenant) {
		t.Errorf("err = %v, want ErrInvalidTenant", err)
	}
}

func TestUpdateContact_RejectsNilContactID(t *testing.T) {
	u := MustNewUpdateContact(newFakeRepo())
	_, err := u.Execute(context.Background(), UpdateContactInput{TenantID: uuid.New(), ContactID: uuid.Nil, DisplayName: "X"})
	if !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateContact_NotFound(t *testing.T) {
	u := MustNewUpdateContact(newFakeRepo())
	_, err := u.Execute(context.Background(), UpdateContactInput{TenantID: uuid.New(), ContactID: uuid.New(), DisplayName: "X"})
	if !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateContact_HappyPath(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	c := seedContact(t, repo, tenant, "Alice")

	u := MustNewUpdateContact(repo)
	res, err := u.Execute(context.Background(), UpdateContactInput{
		TenantID: tenant, ContactID: c.ID, DisplayName: "  Alicia  ",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Contact.DisplayName != "Alicia" {
		t.Errorf("result name = %q, want Alicia (trimmed)", res.Contact.DisplayName)
	}
	// Confirm persisted: re-read through the repo.
	got, err := repo.FindByID(context.Background(), tenant, c.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.DisplayName != "Alicia" {
		t.Errorf("persisted name = %q, want Alicia", got.DisplayName)
	}
}

func TestUpdateContact_RejectsBlankName(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	c := seedContact(t, repo, tenant, "Alice")
	u := MustNewUpdateContact(repo)
	_, err := u.Execute(context.Background(), UpdateContactInput{TenantID: tenant, ContactID: c.ID, DisplayName: "   "})
	if !errors.Is(err, contacts.ErrEmptyDisplayName) {
		t.Errorf("err = %v, want ErrEmptyDisplayName", err)
	}
	// Original name preserved.
	got, _ := repo.FindByID(context.Background(), tenant, c.ID)
	if got.DisplayName != "Alice" {
		t.Errorf("name mutated to %q despite rejected update", got.DisplayName)
	}
}

func TestUpdateContact_CrossTenantHidden(t *testing.T) {
	repo := newFakeRepo()
	tenantA := uuid.New()
	tenantB := uuid.New()
	c := seedContact(t, repo, tenantA, "Alice")
	u := MustNewUpdateContact(repo)
	// Tenant B tries to edit tenant A's contact: collapses to ErrNotFound.
	_, err := u.Execute(context.Background(), UpdateContactInput{TenantID: tenantB, ContactID: c.ID, DisplayName: "Hacked"})
	if !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("cross-tenant err = %v, want ErrNotFound", err)
	}
}

func TestUpdateContact_PropagatesUpdateError(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	c := seedContact(t, repo, tenant, "Alice")
	sentinel := errors.New("synthetic update failure")
	repo.saveErr = sentinel
	u := MustNewUpdateContact(repo)
	_, err := u.Execute(context.Background(), UpdateContactInput{TenantID: tenant, ContactID: c.ID, DisplayName: "Alicia"})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}

func TestUpdateContact_PropagatesFindError(t *testing.T) {
	repo := newFakeRepo()
	sentinel := errors.New("synthetic find failure")
	repo.findErr = sentinel
	u := MustNewUpdateContact(repo)
	_, err := u.Execute(context.Background(), UpdateContactInput{TenantID: uuid.New(), ContactID: uuid.New(), DisplayName: "X"})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}
