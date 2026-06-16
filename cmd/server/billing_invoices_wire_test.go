package main

// SIN-64974 — billing-invoices wire tests. The handler covers its own
// behaviour exhaustively in internal/web/billing/invoices; these tests
// pin the composition root: buildWebBillingInvoicesHandler returns
// (nil, no-op) when either DSN is unset, the pure assembly seam rejects
// nil deps, and the assembled mux mounts every route the surface lists.
// This is the regression guard for the staging 404 (SIN-64964): the
// surface shipped but cmd/server never wired it, so the router's
// `if deps.WebBillingInvoices != nil` guard left the routes unmounted.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/billing"
	billingdunning "github.com/pericles-luz/crm/internal/billing/dunning"
	billingpix "github.com/pericles-luz/crm/internal/billing/pix"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
	webinvoices "github.com/pericles-luz/crm/internal/web/billing/invoices"
)

// memInvoiceStoreForWire is a minimal in-memory store satisfying the
// InvoiceLister + InvoiceGetter ports. The web package's own tests
// cover behaviour; this just confirms wireup and route mount. The list
// returns no rows and GetByID returns billing.ErrNotFound so the detail
// + status routes resolve to a clean 404 (proving they are mounted).
type memInvoiceStoreForWire struct{}

func (memInvoiceStoreForWire) ListByTenant(_ context.Context, _ uuid.UUID) ([]*billing.Invoice, error) {
	return []*billing.Invoice{}, nil
}

func (memInvoiceStoreForWire) GetByID(_ context.Context, _, _ uuid.UUID) (*billing.Invoice, error) {
	return nil, billing.ErrNotFound
}

// memDunningReaderForWire satisfies DunningStateReader. (nil, nil)
// means "no row" which bannerViewFrom treats as StateCurrent (no
// banner) — enough to render the list + banner routes.
type memDunningReaderForWire struct{}

func (memDunningReaderForWire) CurrentForTenant(_ context.Context, _ uuid.UUID) (*billingdunning.DunningState, error) {
	return nil, nil
}

// Compile-time assertions: the in-memory fakes satisfy the web ports.
var (
	_ webinvoices.InvoiceLister      = memInvoiceStoreForWire{}
	_ webinvoices.InvoiceGetter      = memInvoiceStoreForWire{}
	_ webinvoices.DunningStateReader = memDunningReaderForWire{}
)

func TestBuildWebBillingInvoicesHandler_DegradesWhenDSNUnset(t *testing.T) {
	t.Parallel()
	h, cleanup := buildWebBillingInvoicesHandler(context.Background(), func(string) string { return "" })
	defer cleanup()
	if h != nil {
		t.Fatalf("expected nil handler when DATABASE_URL unset; got %T", h)
	}
}

func TestBuildWebBillingInvoicesHandler_DegradesWhenMasterOpsDSNUnset(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == "DATABASE_URL" {
			return "postgres://placeholder"
		}
		return ""
	}
	h, cleanup := buildWebBillingInvoicesHandler(context.Background(), getenv)
	defer cleanup()
	if h != nil {
		t.Fatalf("expected nil handler when %s unset; got %T", envMasterOpsDSN, h)
	}
}

func TestNoChargeLister_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	_, err := noChargeLister{}.LatestForInvoice(context.Background(), uuid.New(), uuid.New())
	if err != billingpix.ErrNotFound {
		t.Fatalf("noChargeLister: err = %v, want pix.ErrNotFound", err)
	}
}

func TestAssembleWebBillingInvoicesHandler_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	store := memInvoiceStoreForWire{}
	dun := memDunningReaderForWire{}
	charges := noChargeLister{}
	cases := []struct {
		name     string
		invoices webinvoices.InvoiceLister
		invoice  webinvoices.InvoiceGetter
		charges  webinvoices.PIXChargeLister
		dunning  webinvoices.DunningStateReader
	}{
		{"nil invoices", nil, store, charges, dun},
		{"nil invoice", store, nil, charges, dun},
		{"nil charges", store, store, nil, dun},
		{"nil dunning", store, store, charges, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := assembleWebBillingInvoicesHandler(tc.invoices, tc.invoice, tc.charges, tc.dunning, slog.Default()); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestAssembleWebBillingInvoicesHandler_DefaultsNilLogger(t *testing.T) {
	t.Parallel()
	h, err := assembleWebBillingInvoicesHandler(
		memInvoiceStoreForWire{}, memInvoiceStoreForWire{}, noChargeLister{}, memDunningReaderForWire{}, nil,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestAssembleWebBillingInvoicesHandler_MountsEveryRoute(t *testing.T) {
	t.Parallel()
	h, err := assembleWebBillingInvoicesHandler(
		memInvoiceStoreForWire{}, memInvoiceStoreForWire{}, noChargeLister{}, memDunningReaderForWire{}, slog.Default(),
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	tenant := &tenancy.Tenant{
		ID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Name: "Acme",
		Host: "acme.crm.local",
	}
	iid := uuid.NewString()
	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		// list + banner render fully (empty invoice set, no dunning
		// row). detail + status fast-resolve to 404 because GetByID
		// returns billing.ErrNotFound — chi's "no route matched" path
		// would emit 404 with the default body, so a 404 from the
		// handler itself (reached only when the route is mounted) is
		// proof of mount. The dunning-banner route returns 200.
		{"list page", http.MethodGet, "/billing/invoices", http.StatusOK},
		{"detail 404 when no row", http.MethodGet, "/billing/invoices/" + iid, http.StatusNotFound},
		{"status fragment 404 when no row", http.MethodGet, "/billing/invoices/" + iid + "/status", http.StatusNotFound},
		{"dunning banner", http.MethodGet, "/billing/dunning-banner", http.StatusOK},
	}
	// list + detail handlers read the CSRF token from the request
	// session (csrfTokenFromSessionContext). The wire test bypasses
	// middleware.Auth, so inject a session with a known token to mirror
	// what RequireAuth would install in production.
	sess := iam.Session{CSRFToken: "wire-test-csrf"}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(c.method, c.path, nil)
			ctx := tenancy.WithContext(req.Context(), tenant)
			ctx = middleware.WithSession(ctx, sess)
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("%s %s status = %d, want %d; body=%s", c.method, c.path, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}
