package main

// SIN-64977 — wire test for the contacts management surface. Proves that
// assembleWebContactsHandlerWith mounts the list route when a
// contacts.Repository is supplied, building the ListContacts/GetDetail/
// UpdateContact use cases off it. Uses in-process fakes (no DB) so it
// runs in the unit suite; the real pgx adapters are integration-tested
// in their own packages.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/iam"
	inboxdomain "github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// fakeContactsRepo is the smallest contacts.Repository: List returns a
// fixed page; the other methods are unused by the list path and return
// not-found so a misroute fails loudly.
type fakeContactsRepo struct {
	items []*contacts.Contact
	total int
}

func (r *fakeContactsRepo) Save(context.Context, *contacts.Contact) error { return nil }
func (r *fakeContactsRepo) FindByID(_ context.Context, _, _ uuid.UUID) (*contacts.Contact, error) {
	return nil, contacts.ErrNotFound
}
func (r *fakeContactsRepo) FindByChannelIdentity(_ context.Context, _ uuid.UUID, _, _ string) (*contacts.Contact, error) {
	return nil, contacts.ErrNotFound
}
func (r *fakeContactsRepo) List(_ context.Context, _ uuid.UUID, _ contacts.ListFilter) ([]*contacts.Contact, int, error) {
	return r.items, r.total, nil
}
func (r *fakeContactsRepo) Update(context.Context, *contacts.Contact) error { return nil }

// fakeConvReader satisfies contactConversationReader.
type fakeConvReader struct{}

func (fakeConvReader) ListConversationsByContact(context.Context, uuid.UUID, uuid.UUID, int) ([]*inboxdomain.Conversation, error) {
	return nil, nil
}

func TestAssembleWebContactsHandlerWith_MountsListRoute(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	c1, _ := contacts.New(tenantID, "Alice Wire")
	repo := &fakeContactsRepo{items: []*contacts.Contact{c1}, total: 1}

	h, err := assembleWebContactsHandlerWith(&fakeIdentitySplitRepo{identity: &contacts.Identity{ID: uuid.New(), TenantID: tenantID}}, repo, fakeConvReader{})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	withCtx := func(r *http.Request) *http.Request {
		ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Host: "tenant.example"})
		ctx = middleware.WithSession(ctx, iam.Session{
			ID: uuid.New(), UserID: uuid.New(), TenantID: tenantID,
			ExpiresAt: time.Now().Add(time.Hour), CSRFToken: "tok-csrf",
		})
		return r.WithContext(ctx)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withCtx(httptest.NewRequest(http.MethodGet, "/contacts", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /contacts status=%d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Alice Wire") {
		t.Errorf("list body missing the contact name; got %q", rec.Body.String())
	}
}

func TestAssembleWebContactsHandlerWith_NilContactsRepoSkipsListRoute(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	h, err := assembleWebContactsHandlerWith(&fakeIdentitySplitRepo{identity: &contacts.Identity{ID: uuid.New(), TenantID: tenantID}}, nil, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/contacts", nil)
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID}))
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /contacts status=%d want 404 (list route must be unmounted without a contacts repo)", rec.Code)
	}
}
