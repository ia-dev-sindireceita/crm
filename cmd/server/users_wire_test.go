package main

// SIN-66496 — assembly-seam test for the tenant user-management wire. Exercises
// assembleWebUsersHandler with an in-memory repository + stub auditor so the
// composition root is covered without a DB / server boot (parallel to
// channels_ui_wire_test.go).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/password"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

type stubUsersRepo struct{ users []tenantusers.User }

func (s *stubUsersRepo) List(context.Context, uuid.UUID) ([]tenantusers.User, error) {
	return s.users, nil
}
func (s *stubUsersRepo) Get(context.Context, uuid.UUID, uuid.UUID) (tenantusers.User, error) {
	return tenantusers.User{}, tenantusers.ErrUserNotFound
}
func (s *stubUsersRepo) Create(context.Context, tenantusers.User, string, tenantusers.Token) error {
	return nil
}
func (s *stubUsersRepo) UpdateRole(context.Context, uuid.UUID, uuid.UUID, iam.Role) (iam.Role, error) {
	return iam.RoleTenantAtendente, nil
}
func (s *stubUsersRepo) Deactivate(context.Context, uuid.UUID, uuid.UUID) (iam.Role, error) {
	return iam.RoleTenantAtendente, nil
}
func (s *stubUsersRepo) Reactivate(context.Context, uuid.UUID, uuid.UUID) (iam.Role, error) {
	return iam.RoleTenantAtendente, nil
}

type stubAuditor struct{}

func (stubAuditor) WriteSecurity(context.Context, audit.SecurityAuditEvent) error { return nil }

func TestAssembleWebUsersHandler_NilRepo(t *testing.T) {
	t.Parallel()
	if _, err := assembleWebUsersHandler(nil, password.Default(), stubAuditor{}, nil); err == nil {
		t.Fatal("want error for nil repo")
	}
}

func TestAssembleWebUsersHandler_ServesList(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	repo := &stubUsersRepo{users: []tenantusers.User{
		{ID: uuid.New(), TenantID: tenantID, Email: "boss@acme.example", Role: iam.RoleTenantGerente},
	}}
	h, err := assembleWebUsersHandler(repo, password.Default(), stubAuditor{}, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/settings/users", nil)
	ctx := iam.WithPrincipal(r.Context(), iam.Principal{UserID: uuid.New(), TenantID: tenantID, Roles: []iam.Role{iam.RoleTenantGerente}})
	ctx = tenancy.WithContext(ctx, &tenancy.Tenant{ID: tenantID, Name: "Acme"})
	h.ServeHTTP(rec, r.WithContext(ctx))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "boss@acme.example") {
		t.Fatalf("list did not render the seeded user: %q", rec.Body.String())
	}
}
