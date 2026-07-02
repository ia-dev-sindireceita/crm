package main

// SIN-66499 — tenant-users wire tests. The handler + domain cover their own
// behaviour exhaustively in internal/web/tenantusers and
// internal/tenantusers; these tests pin the composition root:
// buildWebTenantUsersHandler returns (nil, no-op) when the DSN is unset, the
// pure assembly seam rejects a nil repo/hasher, and the assembled mux mounts
// every route the surface lists.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

// memUsersRepo satisfies tenantusers.Repository with empty results.
type memUsersRepo struct{}

func (memUsersRepo) List(context.Context, uuid.UUID) ([]*tenantusers.User, error) { return nil, nil }
func (memUsersRepo) Get(context.Context, uuid.UUID, uuid.UUID) (*tenantusers.User, error) {
	return nil, tenantusers.ErrNotFound
}
func (memUsersRepo) Create(context.Context, *tenantusers.User) error { return nil }
func (memUsersRepo) UpdateRole(context.Context, uuid.UUID, uuid.UUID, iam.Role) error {
	return tenantusers.ErrNotFound
}
func (memUsersRepo) SetActive(context.Context, uuid.UUID, uuid.UUID, bool) error {
	return tenantusers.ErrNotFound
}
func (memUsersRepo) CountActiveGerentes(context.Context, uuid.UUID) (int, error) { return 0, nil }

type memHasher struct{}

func (memHasher) Hash(plain string) (string, error) { return "argon2id$" + plain, nil }

func TestBuildWebTenantUsersHandler_DegradesWhenDSNUnset(t *testing.T) {
	t.Parallel()
	h, cleanup := buildWebTenantUsersHandler(context.Background(), func(string) string { return "" })
	defer cleanup()
	if h != nil {
		t.Fatalf("expected nil handler when DATABASE_URL unset; got %T", h)
	}
}

func TestAssembleWebTenantUsersHandler_NilPorts(t *testing.T) {
	t.Parallel()
	if _, err := assembleWebTenantUsersHandler(nil, memHasher{}, nil, nil, slog.Default()); err == nil {
		t.Error("expected error for nil repo")
	}
	if _, err := assembleWebTenantUsersHandler(memUsersRepo{}, nil, nil, nil, slog.Default()); err == nil {
		t.Error("expected error for nil hasher")
	}
}

func TestAssembleWebTenantUsersHandler_MountsRoutes(t *testing.T) {
	t.Parallel()
	h, err := assembleWebTenantUsersHandler(memUsersRepo{}, memHasher{}, nil, nil, slog.Default())
	if err != nil {
		t.Fatalf("assemble err = %v", err)
	}
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	req := httptest.NewRequest(http.MethodGet, "/settings/users", nil)
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings/users status = %d, want 200", rec.Code)
	}
}

// tenantUserAuditor build guard: newTenantUserAuditor panics on a nil writer.
func TestNewTenantUserAuditor_NilWriterPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil writer")
		}
	}()
	_ = newTenantUserAuditor(nil, slog.Default())
}
