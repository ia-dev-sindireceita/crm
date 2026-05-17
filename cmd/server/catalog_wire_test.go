package main

// SIN-62907 — catalog wire tests. The handler covers its own behaviour
// exhaustively in internal/web/catalog; these tests pin the composition
// root: buildWebCatalogHandler returns (nil, no-op) when either DSN is
// unset, the pure assembly seam rejects nil deps, and the assembled
// mux mounts every route the description lists.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/catalog"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// memStoreForWire is a minimal in-memory store that satisfies the
// catalogStore union. The web/catalog package's own tests cover
// behaviour; this just confirms wireup and route mount.
type memStoreForWire struct{}

func (memStoreForWire) GetByID(_ context.Context, _, productID uuid.UUID) (*catalog.Product, error) {
	return nil, catalog.ErrNotFound
}
func (memStoreForWire) ListByTenant(_ context.Context, _ uuid.UUID) ([]*catalog.Product, error) {
	return []*catalog.Product{}, nil
}
func (memStoreForWire) SaveProduct(_ context.Context, _ *catalog.Product, _ uuid.UUID) error {
	return nil
}
func (memStoreForWire) DeleteProduct(_ context.Context, _, _, _ uuid.UUID) error {
	return catalog.ErrNotFound
}
func (memStoreForWire) ListByProduct(_ context.Context, _, _ uuid.UUID) ([]*catalog.ProductArgument, error) {
	return nil, nil
}
func (memStoreForWire) SaveArgument(_ context.Context, _ *catalog.ProductArgument, _ uuid.UUID) error {
	return nil
}
func (memStoreForWire) DeleteArgument(_ context.Context, _, _, _ uuid.UUID) error {
	return catalog.ErrNotFound
}

func TestBuildWebCatalogHandler_DegradesWhenDSNUnset(t *testing.T) {
	t.Parallel()
	h, cleanup := buildWebCatalogHandler(context.Background(), func(string) string { return "" })
	defer cleanup()
	if h != nil {
		t.Fatalf("expected nil handler when DATABASE_URL unset; got %T", h)
	}
}

func TestBuildWebCatalogHandler_DegradesWhenMasterOpsDSNUnset(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == "DATABASE_URL" {
			return "postgres://placeholder"
		}
		return ""
	}
	h, cleanup := buildWebCatalogHandler(context.Background(), getenv)
	defer cleanup()
	if h != nil {
		t.Fatalf("expected nil handler when %s unset; got %T", envMasterOpsDSN, h)
	}
}

func TestAssembleWebCatalogHandler_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		store catalogStore
		now   func() time.Time
	}{
		{"nil store", nil, time.Now},
		{"nil now", memStoreForWire{}, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := assembleWebCatalogHandler(tc.store, tc.now, slog.Default()); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestAssembleWebCatalogHandler_DefaultsNilLogger(t *testing.T) {
	t.Parallel()
	h, err := assembleWebCatalogHandler(memStoreForWire{}, time.Now, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestAssembleWebCatalogHandler_MountsEveryRoute(t *testing.T) {
	t.Parallel()
	h, err := assembleWebCatalogHandler(memStoreForWire{}, time.Now, slog.Default())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	tenant := &tenancy.Tenant{
		ID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Name: "Acme",
		Host: "acme.crm.local",
	}
	pid := uuid.NewString()
	aid := uuid.NewString()
	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		// userIDFromSessionContext returns uuid.Nil when no session
		// is in context (the wire test bypasses middleware.Auth), so
		// the delete handlers fast-reject with 401 BEFORE reaching the
		// adapter. That is enough to prove the route is mounted —
		// chi's "no route matched" path emits 404 with the chi-default
		// body, not the handler's "Unauthorized" string.
		{"list page", http.MethodGet, "/catalog", http.StatusOK},
		{"new form", http.MethodGet, "/catalog/new", http.StatusOK},
		{"detail 404 when no row", http.MethodGet, "/catalog/" + pid, http.StatusNotFound},
		{"edit form 404", http.MethodGet, "/catalog/" + pid + "/edit", http.StatusNotFound},
		{"delete fast-rejects on nil actor", http.MethodDelete, "/catalog/" + pid, http.StatusUnauthorized},
		{"new argument form", http.MethodGet, "/catalog/" + pid + "/arguments/new", http.StatusOK},
		{"preview", http.MethodGet, "/catalog/" + pid + "/preview", http.StatusOK},
		{"edit argument 404", http.MethodGet, "/catalog/" + pid + "/arguments/" + aid + "/edit", http.StatusNotFound},
		{"delete argument fast-rejects on nil actor", http.MethodDelete, "/catalog/" + pid + "/arguments/" + aid, http.StatusUnauthorized},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(c.method, c.path, nil)
			req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("%s %s status = %d, want %d; body=%s", c.method, c.path, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}
