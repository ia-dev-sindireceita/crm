package invoices_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/billing/dunning"
	"github.com/pericles-luz/crm/internal/billing/pix"
	"github.com/pericles-luz/crm/internal/tenancy"
	webinvoices "github.com/pericles-luz/crm/internal/web/billing/invoices"
)

// fakeRepo is the in-memory four-port fake the handler tests drive.
// One struct implements InvoiceLister, InvoiceGetter, PIXChargeLister,
// and DunningStateReader so tests can mutate a single fixture.
type fakeRepo struct {
	mu sync.Mutex

	invoices   []*billing.Invoice
	byID       map[uuid.UUID]*billing.Invoice
	charges    map[uuid.UUID]*pix.PIXCharge
	dunningRow *dunning.DunningState

	listErr    error
	getErr     error
	chargeErr  error
	dunningErr error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		byID:    map[uuid.UUID]*billing.Invoice{},
		charges: map[uuid.UUID]*pix.PIXCharge{},
	}
}

func (f *fakeRepo) ListByTenant(_ context.Context, _ uuid.UUID) ([]*billing.Invoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.invoices, nil
}

func (f *fakeRepo) GetByID(_ context.Context, _, invoiceID uuid.UUID) (*billing.Invoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	inv, ok := f.byID[invoiceID]
	if !ok {
		return nil, billing.ErrNotFound
	}
	return inv, nil
}

func (f *fakeRepo) LatestForInvoice(_ context.Context, _, invoiceID uuid.UUID) (*pix.PIXCharge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chargeErr != nil {
		return nil, f.chargeErr
	}
	c, ok := f.charges[invoiceID]
	if !ok {
		return nil, pix.ErrNotFound
	}
	return c, nil
}

func (f *fakeRepo) CurrentForTenant(_ context.Context, _ uuid.UUID) (*dunning.DunningState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dunningErr != nil {
		return nil, f.dunningErr
	}
	return f.dunningRow, nil
}

// seedInvoice constructs a pending invoice for a tenant and registers
// it on the fake. The created invoice is returned so tests can name
// its id in URLs.
func seedInvoice(t *testing.T, f *fakeRepo, tenantID uuid.UUID, period time.Time, amountCents int) *billing.Invoice {
	t.Helper()
	inv, err := billing.NewInvoice(tenantID, uuid.New(),
		period, period.AddDate(0, 1, 0), amountCents, period)
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	f.invoices = append([]*billing.Invoice{inv}, f.invoices...)
	f.byID[inv.ID()] = inv
	return inv
}

// seedCharge constructs a pending PIX charge for the given invoice
// and registers it on the fake. qrCode is the base64-encoded payload
// the UI renders; copyPaste is the EMVCo copia-e-cola string.
func seedCharge(t *testing.T, f *fakeRepo, tenantID uuid.UUID, invoiceID uuid.UUID, qrCode, copyPaste string, now time.Time) *pix.PIXCharge {
	t.Helper()
	c, err := pix.NewCharge(tenantID, invoiceID, qrCode, copyPaste, now.Add(15*time.Minute), now)
	if err != nil {
		t.Fatalf("seed charge: %v", err)
	}
	f.charges[invoiceID] = c
	return c
}

func seedDunning(_ *testing.T, f *fakeRepo, tenantID uuid.UUID, state dunning.State, now time.Time) {
	// HydrateDunningState bypasses the constructor invariants so the
	// test can pin any State directly without walking the full
	// escalate machine; the production cron is what advances the
	// state machine in real life.
	f.dunningRow = dunning.HydrateDunningState(
		uuid.New(),
		tenantID,
		uuid.New(),
		state,
		now,
		uuid.New(),
		nil,
		"",
	)
}

func fullDeps(repo *fakeRepo, actor uuid.UUID, now time.Time) webinvoices.Deps {
	return webinvoices.Deps{
		Invoices:  repo,
		Invoice:   repo,
		Charges:   repo,
		Dunning:   repo,
		CSRFToken: func(*http.Request) string { return "tok" },
		UserID:    func(*http.Request) uuid.UUID { return actor },
		Now:       func() time.Time { return now },
	}
}

func reqWithTenant(method, target string, tenant *tenancy.Tenant) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	return r.WithContext(tenancy.WithContext(r.Context(), tenant))
}

func newTenant() *tenancy.Tenant {
	return &tenancy.Tenant{ID: uuid.New(), Host: "acme.crm.test", Name: "Acme"}
}

func buildHandler(t *testing.T, deps webinvoices.Deps) *webinvoices.Handler {
	t.Helper()
	h, err := webinvoices.New(deps)
	if err != nil {
		t.Fatalf("webinvoices.New: %v", err)
	}
	return h
}

func mux(h *webinvoices.Handler) *http.ServeMux {
	m := http.NewServeMux()
	h.Routes(m)
	return m
}

// ---------------------------------------------------------------------------
// New / construction
// ---------------------------------------------------------------------------

func TestNew_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	base := fullDeps(repo, uuid.New(), time.Unix(0, 0).UTC())
	mutators := map[string]func(*webinvoices.Deps){
		"missing Invoices":  func(d *webinvoices.Deps) { d.Invoices = nil },
		"missing Invoice":   func(d *webinvoices.Deps) { d.Invoice = nil },
		"missing Charges":   func(d *webinvoices.Deps) { d.Charges = nil },
		"missing Dunning":   func(d *webinvoices.Deps) { d.Dunning = nil },
		"missing CSRFToken": func(d *webinvoices.Deps) { d.CSRFToken = nil },
		"missing UserID":    func(d *webinvoices.Deps) { d.UserID = nil },
	}
	for name, mut := range mutators {
		name, mut := name, mut
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := base
			mut(&d)
			if _, err := webinvoices.New(d); err == nil {
				t.Fatalf("New(%s) = nil error, want failure", name)
			}
		})
	}
}

func TestNew_DefaultsApplied(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	deps := webinvoices.Deps{
		Invoices:  repo,
		Invoice:   repo,
		Charges:   repo,
		Dunning:   repo,
		CSRFToken: func(*http.Request) string { return "tok" },
		UserID:    func(*http.Request) uuid.UUID { return uuid.New() },
	}
	h, err := webinvoices.New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := reqWithTenant(http.MethodGet, "/billing/invoices", newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("defaults serve: status=%d body=%s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// list (GET /billing/invoices)
// ---------------------------------------------------------------------------

func TestList_RendersInvoicesAndCSRF(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)

	h := buildHandler(t, fullDeps(repo, uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/billing/invoices", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`<title>Faturas</title>`,
		"05/2026",
		"R$ 49,90",
		`name="csrf-token"`,
		`href="/billing/invoices/`,
		`hx-target="body"`,
		"id=\"dunning-banner\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("list body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestList_EmptyState(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	h := buildHandler(t, fullDeps(repo, uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/billing/invoices", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Nenhuma fatura emitida") {
		t.Errorf("empty-state copy missing: %s", w.Body.String())
	}
}

func TestList_5xxOnRepoErrors(t *testing.T) {
	t.Parallel()
	for name, mut := range map[string]func(*fakeRepo){
		"list error":    func(r *fakeRepo) { r.listErr = errors.New("boom") },
		"dunning error": func(r *fakeRepo) { r.dunningErr = errors.New("boom") },
	} {
		name, mut := name, mut
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			mut(repo)
			h := buildHandler(t, fullDeps(repo, uuid.New(), time.Now().UTC()))
			r := reqWithTenant(http.MethodGet, "/billing/invoices", newTenant())
			w := httptest.NewRecorder()
			mux(h).ServeHTTP(w, r)
			if w.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", w.Code)
			}
		})
	}
}

func TestList_5xxOnMissingCSRF(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	deps := fullDeps(repo, uuid.New(), time.Now().UTC())
	deps.CSRFToken = func(*http.Request) string { return "" }
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/billing/invoices", newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestList_5xxOnMissingTenant(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := buildHandler(t, fullDeps(repo, uuid.New(), time.Now().UTC()))
	r := httptest.NewRequest(http.MethodGet, "/billing/invoices", nil)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// ---------------------------------------------------------------------------
// detail (GET /billing/invoices/{id})
// ---------------------------------------------------------------------------

func TestDetail_RendersInvoiceWithPIXChargeAndCopyPaste(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	inv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)
	const (
		// Tiny SVG payload base64-encoded; the handler must inline
		// it as data:image/svg+xml;base64,…
		svgB64    = "PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciLz4="
		copyPaste = "00020126360014BR.GOV.BCB.PIX0114+5511999998888520400005303986540510.005802BR5913Fulano de Tal6008BRASILIA62070503***6304ABCD"
	)
	seedCharge(t, repo, tenant.ID, inv.ID(), svgB64, copyPaste, now)

	h := buildHandler(t, fullDeps(repo, uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/billing/invoices/"+inv.ID().String(), tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// html/template HTML-encodes `+` to `&#43;` inside attribute /
	// textarea content; the browser decodes both transparently, so we
	// assert against the encoded form.
	for _, want := range []string{
		"Fatura 05/2026",
		`<img src="data:image/svg&#43;xml;base64,` + svgB64,
		`00020126360014BR.GOV.BCB.PIX0114&#43;5511999998888520400005303986540510.005802BR5913Fulano de Tal6008BRASILIA62070503***6304ABCD`,
		`data-copy-target="#invoice-copy-paste"`,
		`hx-get="/billing/invoices/` + inv.ID().String() + `/status"`,
		`hx-trigger="every 10s"`, // AC #3 — poll only while pending
		`aguardando pagamento`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestDetail_RendersPlaceholderWhenChargePending(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	inv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)

	h := buildHandler(t, fullDeps(repo, uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/billing/invoices/"+inv.ID().String(), tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Cobrança PIX em processamento") {
		t.Errorf("pending placeholder missing")
	}
	if strings.Contains(body, "<img src=\"data:") {
		t.Errorf("placeholder should not render an img tag, got body: %s", body)
	}
	if !strings.Contains(body, `hx-trigger="every 10s"`) {
		t.Errorf("polling must remain active when no charge yet")
	}
}

func TestDetail_404ForUnknownInvoice(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	h := buildHandler(t, fullDeps(repo, uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/billing/invoices/"+uuid.NewString(), tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestDetail_400ForInvalidInvoiceID(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	h := buildHandler(t, fullDeps(repo, uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/billing/invoices/not-a-uuid", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestDetail_5xxOnRepoErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*fakeRepo){
		"get invoice error": func(r *fakeRepo) { r.getErr = errors.New("boom") },
		"charge error":      func(r *fakeRepo) { r.chargeErr = errors.New("boom") },
		"dunning error":     func(r *fakeRepo) { r.dunningErr = errors.New("boom") },
	}
	for name, mut := range cases {
		name, mut := name, mut
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			tenant := newTenant()
			inv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)
			mut(repo)
			h := buildHandler(t, fullDeps(repo, uuid.New(), time.Now().UTC()))
			r := reqWithTenant(http.MethodGet, "/billing/invoices/"+inv.ID().String(), tenant)
			w := httptest.NewRecorder()
			mux(h).ServeHTTP(w, r)
			if w.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", w.Code)
			}
		})
	}
}

func TestDetail_5xxOnMissingCSRF(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	inv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)
	deps := fullDeps(repo, uuid.New(), time.Now().UTC())
	deps.CSRFToken = func(*http.Request) string { return "" }
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/billing/invoices/"+inv.ID().String(), tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// ---------------------------------------------------------------------------
// status fragment (GET /billing/invoices/{id}/status)
// ---------------------------------------------------------------------------

func TestStatusFragment_PendingKeepsPolling(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	inv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)
	seedCharge(t, repo, tenant.ID, inv.ID(), "PHN2Zy8+", "copypaste", now)

	h := buildHandler(t, fullDeps(repo, uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/billing/invoices/"+inv.ID().String()+"/status", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-trigger="every 10s"`,
		`invoice-pix-status--pending`,
		`aguardando pagamento`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pending fragment missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestStatusFragment_TerminalStopsPolling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		mutate   func(*pix.PIXCharge, time.Time)
		wantHTML string
		wantCSS  string
	}{
		{
			name:     "paid",
			mutate:   func(c *pix.PIXCharge, now time.Time) { _, _ = c.MarkPaid(now) },
			wantHTML: "pago",
			wantCSS:  "invoice-pix-status--paid",
		},
		{
			name: "expired",
			mutate: func(c *pix.PIXCharge, _ time.Time) {
				_, _ = c.Expire(c.ExpiresAt().Add(time.Minute))
			},
			wantHTML: "expirado",
			wantCSS:  "invoice-pix-status--expired",
		},
		{
			name:     "cancelled",
			mutate:   func(c *pix.PIXCharge, now time.Time) { _, _ = c.Cancel(now) },
			wantHTML: "cancelado",
			wantCSS:  "invoice-pix-status--cancelled",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			tenant := newTenant()
			now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
			inv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)
			c := seedCharge(t, repo, tenant.ID, inv.ID(), "PHN2Zy8+", "copypaste", now)
			tc.mutate(c, now)

			h := buildHandler(t, fullDeps(repo, uuid.New(), now))
			r := reqWithTenant(http.MethodGet, "/billing/invoices/"+inv.ID().String()+"/status", tenant)
			w := httptest.NewRecorder()
			mux(h).ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			body := w.Body.String()
			if strings.Contains(body, `hx-trigger`) {
				t.Errorf("terminal status must omit hx-trigger; got: %s", body)
			}
			if !strings.Contains(body, tc.wantHTML) {
				t.Errorf("label %q missing in body: %s", tc.wantHTML, body)
			}
			if !strings.Contains(body, tc.wantCSS) {
				t.Errorf("css class %q missing in body: %s", tc.wantCSS, body)
			}
		})
	}
}

func TestStatusFragment_NoChargeYetRendersPendingAndPolls(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	inv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)

	h := buildHandler(t, fullDeps(repo, uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/billing/invoices/"+inv.ID().String()+"/status", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `hx-trigger="every 10s"`) {
		t.Errorf("no-charge-yet must still poll: %s", body)
	}
	if !strings.Contains(body, "aguardando pagamento") {
		t.Errorf("no-charge-yet must render pending label: %s", body)
	}
}

func TestStatusFragment_404ForUnknownInvoice(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	h := buildHandler(t, fullDeps(repo, uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/billing/invoices/"+uuid.NewString()+"/status", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestStatusFragment_400ForInvalidID(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	h := buildHandler(t, fullDeps(repo, uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/billing/invoices/not-a-uuid/status", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestStatusFragment_5xxOnChargeError(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	inv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)
	repo.chargeErr = errors.New("boom")
	h := buildHandler(t, fullDeps(repo, uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/billing/invoices/"+inv.ID().String()+"/status", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// ---------------------------------------------------------------------------
// dunning banner fragment (GET /billing/dunning-banner)
// ---------------------------------------------------------------------------

func TestBannerFragment_HiddenWhenCurrent(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	h := buildHandler(t, fullDeps(repo, uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/billing/dunning-banner", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "dunning-banner--hidden") {
		t.Errorf("clean tenant should render hidden banner; got: %s", body)
	}
	if strings.Contains(body, "role=\"alert\"") {
		t.Errorf("hidden banner should not carry role=alert; got: %s", body)
	}
}

func TestBannerFragment_PerSeverity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		state    dunning.State
		severity string
		label    string
	}{
		// D1 4-state copy updated in SIN-63944 (task spec = CTO auth per rule 3).
		{"warn", dunning.StateWarn, "dunning-banner--warn", "Fatura próxima do vencimento"},
		{"outbound", dunning.StateSuspendedOutbound, "dunning-banner--outbound", "Fatura em atraso"},
		{"full", dunning.StateSuspendedFull, "dunning-banner--full", "Acesso restrito"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			tenant := newTenant()
			now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
			seedDunning(t, repo, tenant.ID, tc.state, now)
			h := buildHandler(t, fullDeps(repo, uuid.New(), now))
			r := reqWithTenant(http.MethodGet, "/billing/dunning-banner", tenant)
			w := httptest.NewRecorder()
			mux(h).ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			body := w.Body.String()
			for _, want := range []string{tc.severity, tc.label, `role="alert"`} {
				if !strings.Contains(body, want) {
					t.Errorf("banner missing %q\n--- body ---\n%s", want, body)
				}
			}
		})
	}
}

func TestBannerFragment_HiddenWhenOverrideActive(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	seedDunning(t, repo, tenant.ID, dunning.StateWarn, now)
	if err := repo.dunningRow.ApplyOverride(now.AddDate(0, 0, 7), "courtesy free period — onboarding", now); err != nil {
		t.Fatalf("apply override: %v", err)
	}
	h := buildHandler(t, fullDeps(repo, uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/billing/dunning-banner", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, "dunning-banner--hidden") {
		t.Errorf("active override should hide banner; got: %s", body)
	}
}

func TestBannerFragment_5xxOnDunningError(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.dunningErr = errors.New("boom")
	tenant := newTenant()
	h := buildHandler(t, fullDeps(repo, uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/billing/dunning-banner", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestBannerFragment_5xxOnMissingTenant(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := buildHandler(t, fullDeps(repo, uuid.New(), time.Now().UTC()))
	r := httptest.NewRequest(http.MethodGet, "/billing/dunning-banner", nil)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// ---------------------------------------------------------------------------
// D1 4-state dunning banner — new states (SIN-63944)
// ---------------------------------------------------------------------------

func TestBannerFragment_BloqueioState(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	// 15 days since entering StateSuspendedFull → "bloqueio" (>14d)
	enteredAt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	now := enteredAt.AddDate(0, 0, 15)
	seedDunning(t, repo, tenant.ID, dunning.StateSuspendedFull, enteredAt)
	h := buildHandler(t, fullDeps(repo, uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/billing/dunning-banner", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"dunning-banner--bloqueio",
		"alert--danger",
		"Conta bloqueada",
		"regularizar agora",
		`role="alert"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bloqueio banner missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestBannerFragment_RestricaoState(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	// 10 days since entering StateSuspendedFull → "restricao" (≤14d)
	enteredAt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	now := enteredAt.AddDate(0, 0, 10)
	seedDunning(t, repo, tenant.ID, dunning.StateSuspendedFull, enteredAt)
	h := buildHandler(t, fullDeps(repo, uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/billing/dunning-banner", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"dunning-banner--full",
		"alert--danger",
		"Acesso restrito",
		`role="alert"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("restricao banner missing %q\n--- body ---\n%s", want, body)
		}
	}
	if strings.Contains(body, "regularizar agora") {
		t.Errorf("restricao banner must not show 'regularizar agora' CTA")
	}
}

func TestBannerFragment_AlertClassesPresent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		state     dunning.State
		wantClass string
	}{
		{"warn→info", dunning.StateWarn, "alert--info"},
		{"outbound→warning", dunning.StateSuspendedOutbound, "alert--warning"},
		{"full→danger", dunning.StateSuspendedFull, "alert--danger"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			tenant := newTenant()
			now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
			seedDunning(t, repo, tenant.ID, tc.state, now)
			h := buildHandler(t, fullDeps(repo, uuid.New(), now))
			r := reqWithTenant(http.MethodGet, "/billing/dunning-banner", tenant)
			w := httptest.NewRecorder()
			mux(h).ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			if !strings.Contains(w.Body.String(), tc.wantClass) {
				t.Errorf("missing F1 alert class %q in: %s", tc.wantClass, w.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Invoice list filters (SIN-63944 AC #3)
// ---------------------------------------------------------------------------

func TestList_FilterByStatus(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	pendingInv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)
	paidInv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), 4990)
	if err := paidInv.MarkPaid(now); err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}

	h := buildHandler(t, fullDeps(repo, uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/billing/invoices?status=pending", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, pendingInv.ID().String()) {
		t.Errorf("pending invoice missing from filter result")
	}
	if strings.Contains(body, paidInv.ID().String()) {
		t.Errorf("paid invoice should be excluded when filtering by pending")
	}
}

func TestList_FilterByPeriod(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	mayInv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)
	aprInv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), 4990)

	h := buildHandler(t, fullDeps(repo, uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/billing/invoices?period=2026-05", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, mayInv.ID().String()) {
		t.Errorf("May invoice missing from period=2026-05 filter")
	}
	if strings.Contains(body, aprInv.ID().String()) {
		t.Errorf("April invoice should be excluded from period=2026-05 filter")
	}
}

func TestList_HTMXPartialSwapReturnsTbodyOnly(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)

	h := buildHandler(t, fullDeps(repo, uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/billing/invoices", tenant)
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<html") {
		t.Errorf("HTMX partial should not return full HTML page; got: %s", body)
	}
	if !strings.Contains(body, "invoice-row") {
		t.Errorf("HTMX partial missing invoice row: %s", body)
	}
}

func TestList_EmptyStateWithNextBillingDate(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	nextBilling := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	deps := fullDeps(repo, uuid.New(), now)
	deps.NextBillingDate = func(_ context.Context, _ uuid.UUID) (time.Time, error) {
		return nextBilling, nil
	}
	h := buildHandler(t, deps)

	r := reqWithTenant(http.MethodGet, "/billing/invoices", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Nenhuma fatura emitida") {
		t.Errorf("empty-state missing: %s", body)
	}
	if !strings.Contains(body, "01/06/2026") {
		t.Errorf("next billing date missing (want 01/06/2026): %s", body)
	}
}

// ---------------------------------------------------------------------------
// Mobile PIX markup (SIN-63944 AC #2)
// ---------------------------------------------------------------------------

func TestDetail_MobilePIXMarkup(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	inv := seedInvoice(t, repo, tenant.ID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 4990)
	seedCharge(t, repo, tenant.ID, inv.ID(), "PHN2Zy8+", "pix-code", now)

	h := buildHandler(t, fullDeps(repo, uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/billing/invoices/"+inv.ID().String(), tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="invoice-pix__copy-btn"`,
		`billing-invoices.css`,
		`width="240"`,
		`height="240"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mobile PIX markup missing %q\n--- body ---\n%s", want, body)
		}
	}
}
