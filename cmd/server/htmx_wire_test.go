package main

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
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

func TestBuildWebContactsHandler_DisabledWhenDSNUnset(t *testing.T) {
	t.Parallel()
	h, cleanup := buildWebContactsHandler(context.Background(), func(string) string { return "" })
	defer cleanup()
	if h != nil {
		t.Fatalf("expected nil handler when DATABASE_URL unset, got %T", h)
	}
}

// fakeIdentitySplitRepo is the smallest contactsusecase.IdentitySplitRepository
// implementation; it returns a fixed identity for FindByContactID and
// records Split calls. No DB, no goroutines.
type fakeIdentitySplitRepo struct {
	identity *contacts.Identity
	findErr  error

	splitCalls []uuid.UUID
	splitErr   error
}

func (r *fakeIdentitySplitRepo) FindByContactID(_ context.Context, _, _ uuid.UUID) (*contacts.Identity, error) {
	if r.findErr != nil {
		return nil, r.findErr
	}
	return r.identity, nil
}

func (r *fakeIdentitySplitRepo) Split(_ context.Context, _, linkID uuid.UUID) error {
	r.splitCalls = append(r.splitCalls, linkID)
	return r.splitErr
}

func TestAssembleWebContactsHandler_RejectsNilRepo(t *testing.T) {
	t.Parallel()
	if _, err := assembleWebContactsHandler(nil); err == nil {
		t.Fatalf("expected error on nil repo, got nil")
	}
}

func TestAssembleWebContactsHandler_RegistersBothRoutes(t *testing.T) {
	t.Parallel()
	contactID := uuid.New()
	tenantID := uuid.New()
	identity := &contacts.Identity{ID: uuid.New(), TenantID: tenantID}
	repo := &fakeIdentitySplitRepo{identity: identity}

	h, err := assembleWebContactsHandler(repo)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if h == nil {
		t.Fatalf("expected non-nil handler")
	}

	// Wrap the handler with the same context the production chain
	// (TenantScope + Auth) installs so the inner handler succeeds.
	withCtx := func(r *http.Request) *http.Request {
		ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Host: "tenant.example"})
		ctx = middleware.WithSession(ctx, iam.Session{
			ID:        uuid.New(),
			UserID:    uuid.New(),
			TenantID:  tenantID,
			ExpiresAt: time.Now().Add(time.Hour),
			CSRFToken: "tok-csrf",
		})
		return r.WithContext(ctx)
	}

	t.Run("GET /contacts/{id} renders fragment", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := withCtx(httptest.NewRequest(http.MethodGet, "/contacts/"+contactID.String(), nil))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("content-type = %q, want text/html", ct)
		}
	})

	t.Run("POST /contacts/identity/split delegates to use case", func(t *testing.T) {
		linkID := uuid.New()
		body := "link_id=" + linkID.String() + "&survivor_contact_id=" + contactID.String()
		req := httptest.NewRequest(http.MethodPost, "/contacts/identity/split", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = withCtx(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if len(repo.splitCalls) != 1 || repo.splitCalls[0] != linkID {
			t.Fatalf("split calls = %v, want [%s]", repo.splitCalls, linkID)
		}
	})

	t.Run("GET /contacts/{id} surfaces ErrNotFound as 404", func(t *testing.T) {
		notFound := &fakeIdentitySplitRepo{findErr: contacts.ErrNotFound}
		h2, err := assembleWebContactsHandler(notFound)
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		rec := httptest.NewRecorder()
		req := withCtx(httptest.NewRequest(http.MethodGet, "/contacts/"+contactID.String(), nil))
		h2.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestCSRFTokenFromSessionContext_ReturnsTokenFromSession(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(middleware.WithSession(r.Context(), iam.Session{CSRFToken: "tok-xyz"}))
	if got := csrfTokenFromSessionContext(r); got != "tok-xyz" {
		t.Fatalf("token = %q, want tok-xyz", got)
	}
}

func TestCSRFTokenFromSessionContext_EmptyWhenNoSession(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := csrfTokenFromSessionContext(r); got != "" {
		t.Fatalf("token = %q, want empty", got)
	}
}

// Compile-time verification that the wire's assemble result satisfies
// httpapi.Deps.WebContacts (which is http.Handler) and that the fake
// repo satisfies the IdentitySplitRepository contract.
var (
	_ http.Handler                            = (*http.ServeMux)(nil)
	_ contactsusecase.IdentitySplitRepository = (*fakeIdentitySplitRepo)(nil)
)
