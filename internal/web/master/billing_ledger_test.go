package master_test

// SIN-62885 / Fase 2.5 C11 — handler tests for the master billing
// history + token ledger views.
//
// AC coverage:
//
//   AC #1 — grants ordered desc by created_at with active/revoked/
//           consumed status: TestShowBilling_RendersThreePanels +
//           TestShowBilling_GrantStatusLabels.
//   AC #2 — ledger paginated with cursor: TestShowLedger_CursorRoundTrip
//           + TestShowLedger_PageSizeClamped + the load-more rendering
//           in TestShowLedger_RendersLoadMoreWhenHasMore.
//   AC #3 — RLS isolation (gerente do tenant Y NÃO vê tenant X):
//           TestShowBilling_GerenteDifferentTenantSeesEmpty +
//           TestShowLedger_GerenteDifferentTenantSeesEmpty. The
//           in-process fake stands in for the WithTenant boundary —
//           it filters by the authoritative tenantID passed by the
//           handler, exactly the way the WithTenant-scoped runtime
//           pool does at the SQL boundary.
//   AC #4 — accessibility: TestShowBilling_AccessibleStructure +
//           TestShowLedger_AccessibleStructure.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/web/master"
)

// ----- Stubs -----------------------------------------------------------

// stubBilling implements master.BillingViewer. It mirrors the
// WithTenant-scoped adapter: returns the seeded view for `tenantID`
// AND zero-rows for any other tenant id. That replicates the RLS
// guarantee in-process so the handler test can prove AC #3.
type stubBilling struct {
	tenantID uuid.UUID
	view     master.BillingView
	err      error
	calls    int
}

func (s *stubBilling) ViewBilling(_ context.Context, tenantID uuid.UUID) (master.BillingView, error) {
	s.calls++
	if s.err != nil {
		return master.BillingView{}, s.err
	}
	if tenantID != s.tenantID {
		// Cross-tenant access — the WithTenant-scoped runtime pool's
		// RLS policy filters every row out. The handler MUST surface
		// that as an empty view, NOT as a 5xx.
		return master.BillingView{TenantID: tenantID}, nil
	}
	return s.view, nil
}

// stubLedger implements master.LedgerViewer with the same tenant
// filter as stubBilling. It honours the cursor + page size to make
// the cursor round-trip test meaningful.
type stubLedger struct {
	tenantID uuid.UUID
	rows     []master.LedgerRow
	err      error
	lastOpts master.LedgerOptions
}

func (s *stubLedger) ViewLedger(_ context.Context, opts master.LedgerOptions) (master.LedgerPage, error) {
	s.lastOpts = opts
	if s.err != nil {
		return master.LedgerPage{}, s.err
	}
	if opts.TenantID != s.tenantID {
		return master.LedgerPage{}, nil
	}
	// Apply the cursor: keep rows STRICTLY before (occurred_at,id) DESC.
	filtered := make([]master.LedgerRow, 0, len(s.rows))
	for _, row := range s.rows {
		if opts.CursorOccurredAt.IsZero() {
			filtered = append(filtered, row)
			continue
		}
		if row.OccurredAt.Before(opts.CursorOccurredAt) {
			filtered = append(filtered, row)
			continue
		}
		if row.OccurredAt.Equal(opts.CursorOccurredAt) && row.ID.String() < opts.CursorID.String() {
			filtered = append(filtered, row)
			continue
		}
	}
	page := master.LedgerPage{}
	if opts.PageSize > 0 && len(filtered) > opts.PageSize {
		page.Entries = filtered[:opts.PageSize]
		page.HasMore = true
		last := page.Entries[len(page.Entries)-1]
		page.NextCursorOccurredAt = last.OccurredAt
		page.NextCursorID = last.ID
	} else {
		page.Entries = filtered
	}
	return page, nil
}

// ----- Fixtures --------------------------------------------------------

func acmeTenantID() uuid.UUID {
	return uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
}

func otherTenantID() uuid.UUID {
	return uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
}

func sampleSubscription() master.SubscriptionRow {
	return master.SubscriptionRow{
		ID:                 uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		PlanID:             uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		PlanSlug:           "pro",
		PlanName:           "Pro",
		PlanPriceCentsBRL:  19_900,
		Status:             "active",
		CurrentPeriodStart: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		CurrentPeriodEnd:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		NextInvoiceAt:      time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
}

func sampleInvoices() []master.InvoiceRow {
	return []master.InvoiceRow{
		{
			ID:             uuid.MustParse("c0000000-0000-0000-0000-000000000001"),
			PeriodStart:    time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			PeriodEnd:      time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			AmountCentsBRL: 19_900,
			State:          "paid",
		},
		{
			ID:             uuid.MustParse("c0000000-0000-0000-0000-000000000002"),
			PeriodStart:    time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
			PeriodEnd:      time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			AmountCentsBRL: 19_900,
			State:          "cancelled_by_master",
		},
	}
}

func sampleGrants() []master.GrantRow {
	revokedAt := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	consumedAt := time.Date(2026, 3, 20, 8, 0, 0, 0, time.UTC)
	return []master.GrantRow{
		{
			ID:          uuid.MustParse("9aaaaaaa-0000-0000-0000-000000000001"),
			ExternalID:  "01HZGRANT0001",
			TenantID:    acmeTenantID(),
			Kind:        master.GrantKindExtraTokens,
			Amount:      500_000,
			Reason:      "Compensação por incidente do dia 03/04",
			CreatedByID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			CreatedAt:   time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC),
		},
		{
			ID:          uuid.MustParse("9aaaaaaa-0000-0000-0000-000000000002"),
			ExternalID:  "01HZGRANT0002",
			TenantID:    acmeTenantID(),
			Kind:        master.GrantKindFreeSubscriptionPeriod,
			PeriodDays:  30,
			Reason:      "Período de teste estendido para parceiro estratégico",
			CreatedByID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			CreatedAt:   time.Date(2026, 4, 8, 9, 0, 0, 0, time.UTC),
			Revoked:     true,
			RevokedAt:   revokedAt,
		},
		{
			ID:          uuid.MustParse("9aaaaaaa-0000-0000-0000-000000000003"),
			ExternalID:  "01HZGRANT0003",
			TenantID:    acmeTenantID(),
			Kind:        master.GrantKindExtraTokens,
			Amount:      100_000,
			Reason:      "Bonificação onboarding (consumida na primeira reserva)",
			CreatedByID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			CreatedAt:   time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC),
			Consumed:    true,
			ConsumedAt:  consumedAt,
		},
	}
}

// ledgerFixture builds n synthetic ledger rows with descending
// timestamps so the cursor pagination has predictable semantics.
func ledgerFixture(n int) []master.LedgerRow {
	base := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	out := make([]master.LedgerRow, 0, n)
	for i := 0; i < n; i++ {
		row := master.LedgerRow{
			ID:             uuid.MustParse(synthLedgerUUID(i)),
			OccurredAt:     base.Add(-time.Duration(i) * time.Minute),
			CreatedAt:      base.Add(-time.Duration(i) * time.Minute),
			Source:         "consumption",
			Kind:           "commit",
			Amount:         -100,
			ExternalRef:    "wamid:" + synthShort(i),
			IdempotencyKey: "rsv:" + synthShort(i),
		}
		switch i % 5 {
		case 0:
			row.Source = "master_grant"
			row.Kind = "grant"
			row.Amount = 100_000
			row.MasterGrantID = uuid.MustParse("9aaaaaaa-0000-0000-0000-000000000001")
			row.MasterGrantExternalID = "01HZGRANT0001"
			row.ExternalRef = ""
		case 1:
			row.Source = "monthly_alloc"
			row.Kind = "grant"
			row.Amount = 1_000_000
			row.SubscriptionID = uuid.MustParse("11111111-2222-3333-4444-555555555555")
			row.SubscriptionPlanSlug = "pro"
			row.ExternalRef = ""
		}
		out = append(out, row)
	}
	return out
}

// synthLedgerUUID returns a deterministic UUID for fixture row i.
func synthLedgerUUID(i int) string {
	// Reserve 0xabcd... namespace for fixtures so a real-row collision
	// is essentially impossible.
	return "abcdabcd-0000-0000-0000-" + synthHex12(i)
}

func synthShort(i int) string { return synthHex12(i) }
func synthHex12(i int) string {
	const hex = "0123456789abcdef"
	var buf [12]byte
	for j := 11; j >= 0; j-- {
		buf[j] = hex[i&0xf]
		i >>= 4
	}
	return string(buf[:])
}

// ----- Helpers ---------------------------------------------------------

func masterPrincipal_C11() iam.Principal {
	return iam.Principal{
		UserID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TenantID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Roles:    []iam.Role{iam.RoleMaster},
	}
}

func reqWithMasterC11(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	return r.WithContext(iam.WithPrincipal(r.Context(), masterPrincipal_C11()))
}

// gerentePrincipalForTenant returns a non-master principal scoped to
// the supplied tenant. Used by AC #3 cross-tenant tests.
func gerentePrincipalForTenant(tenantID uuid.UUID) iam.Principal {
	return iam.Principal{
		UserID:   uuid.MustParse("22222222-3333-4444-5555-666666666666"),
		TenantID: tenantID,
		Roles:    []iam.Role{iam.RoleTenantGerente},
	}
}

// reqWithGerenteC11 builds a request authenticated as a gerente of
// `principalTenant`. AC #3 uses this to prove cross-tenant URLs 403.
func reqWithGerenteC11(method, target string, principalTenant uuid.UUID) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	return r.WithContext(iam.WithPrincipal(r.Context(), gerentePrincipalForTenant(principalTenant)))
}

func newC11Handler(t *testing.T, billing master.BillingViewer, ledger master.LedgerViewer) (*master.Handler, *http.ServeMux) {
	t.Helper()
	deps := master.Deps{
		Tenants:   &stubLister{res: master.ListResult{Tenants: []master.TenantRow{acmeRow()}, Page: 1, PageSize: 25, TotalCount: 1}},
		Creator:   &stubCreator{res: master.CreateTenantResult{Tenant: acmeRow()}},
		Plans:     &stubPlans{},
		Assigner:  &stubAssigner{},
		CSRFToken: func(*http.Request) string { return "csrf-test-token" },
		Logger:    discardLogger(),
		Billing:   billing,
		Ledger:    ledger,
	}
	h, err := master.New(deps)
	if err != nil {
		t.Fatalf("master.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return h, mux
}

func sampleBillingView() master.BillingView {
	return master.BillingView{
		TenantID:     acmeTenantID(),
		Subscription: sampleSubscription(),
		Invoices:     sampleInvoices(),
		Grants:       sampleGrants(),
	}
}

// ----- New (constructor) -----------------------------------------------

func TestNew_RejectsInvalidLedgerPageSizeConfig(t *testing.T) {
	d := master.Deps{
		Tenants:               &stubLister{},
		Creator:               &stubCreator{},
		Plans:                 &stubPlans{},
		Assigner:              &stubAssigner{},
		CSRFToken:             func(*http.Request) string { return "x" },
		LedgerDefaultPageSize: 500,
		LedgerMaxPageSize:     100,
	}
	if _, err := master.New(d); err == nil {
		t.Fatalf("expected LedgerDefaultPageSize > LedgerMaxPageSize to fail")
	}
}

// ----- ShowBilling -----------------------------------------------------

func TestShowBilling_RendersThreePanels(t *testing.T) {
	billing := &stubBilling{tenantID: acmeTenantID(), view: sampleBillingView()}
	_, mux := newC11Handler(t, billing, nil)

	req := reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/billing")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<!doctype html>",
		`id="master-billing-title"`,
		`id="billing-panel"`,
		// 3 panels
		`id="billing-sub-title"`,
		`id="billing-inv-title"`,
		`id="billing-grants-title"`,
		// subscription content
		"Pro",
		`data-plan-slug="pro"`,
		`data-sub-status="active"`,
		// invoice content
		`data-invoice-state="paid"`,
		`data-invoice-state="cancelled_by_master"`,
		// grant content (all three statuses present, AC #1)
		`data-grant-state="active"`,
		`data-grant-state="revoked"`,
		`data-grant-state="consumed"`,
		// crumb links
		`href="/master/tenants"`,
		`href="/master/tenants/` + acmeTenantID().String() + `/ledger"`,
		// CSRF surface
		"csrf-test-token",
		"hx-headers=",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestShowBilling_GrantOrderingPreservedDescByCreatedAt(t *testing.T) {
	// AC #1: grants ordered desc by created_at. The adapter is
	// responsible for the sort; this test pins the handler's "render
	// in adapter-supplied order" contract so a future regression that
	// re-sorts in the template gets caught.
	billing := &stubBilling{tenantID: acmeTenantID(), view: sampleBillingView()}
	_, mux := newC11Handler(t, billing, nil)

	req := reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/billing")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	idx1 := strings.Index(body, "9aaaaaaa-0000-0000-0000-000000000001")
	idx2 := strings.Index(body, "9aaaaaaa-0000-0000-0000-000000000002")
	idx3 := strings.Index(body, "9aaaaaaa-0000-0000-0000-000000000003")
	if idx1 < 0 || idx2 < 0 || idx3 < 0 {
		// Grant rows render by ID — find them by data-grant-id attr.
		t.Fatalf("grant rows missing from body: %d %d %d", idx1, idx2, idx3)
	}
	if !(idx1 < idx2 && idx2 < idx3) {
		t.Errorf("grants not in desc-by-created_at order: idx1=%d idx2=%d idx3=%d", idx1, idx2, idx3)
	}
}

func TestShowBilling_EmptyTenantRendersEmptyState(t *testing.T) {
	billing := &stubBilling{tenantID: acmeTenantID(), view: master.BillingView{TenantID: acmeTenantID()}}
	_, mux := newC11Handler(t, billing, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/billing"))

	body := rec.Body.String()
	if !strings.Contains(body, "Tenant sem assinatura ativa") {
		t.Errorf("expected empty subscription state; body=%s", body)
	}
	if !strings.Contains(body, "Nenhum invoice emitido") {
		t.Errorf("expected empty invoices state")
	}
	if !strings.Contains(body, "Nenhuma cortesia emitida") {
		t.Errorf("expected empty grants state")
	}
}

func TestShowBilling_HXRequestRendersPartial(t *testing.T) {
	billing := &stubBilling{tenantID: acmeTenantID(), view: sampleBillingView()}
	_, mux := newC11Handler(t, billing, nil)

	req := reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/billing")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("HX-Request should yield panel-only partial")
	}
	if !strings.Contains(body, `id="billing-panel"`) {
		t.Errorf("partial missing #billing-panel marker")
	}
}

func TestShowBilling_InvalidUUID(t *testing.T) {
	billing := &stubBilling{tenantID: acmeTenantID(), view: sampleBillingView()}
	_, mux := newC11Handler(t, billing, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/not-a-uuid/billing"))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestShowBilling_PortNotFound(t *testing.T) {
	billing := &stubBilling{tenantID: acmeTenantID(), err: master.ErrTenantNotFound}
	_, mux := newC11Handler(t, billing, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/billing"))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestShowBilling_PortError(t *testing.T) {
	billing := &stubBilling{tenantID: acmeTenantID(), err: errors.New("kaboom")}
	_, mux := newC11Handler(t, billing, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/billing"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestShowBilling_MissingPrincipalIs401(t *testing.T) {
	billing := &stubBilling{tenantID: acmeTenantID(), view: sampleBillingView()}
	_, mux := newC11Handler(t, billing, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/billing", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestShowBilling_PortNotConfiguredIs503(t *testing.T) {
	_, mux := newC11Handler(t, nil, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/billing"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestShowBilling_EmptyCSRFIs500(t *testing.T) {
	billing := &stubBilling{tenantID: acmeTenantID(), view: sampleBillingView()}
	deps := master.Deps{
		Tenants:   &stubLister{},
		Creator:   &stubCreator{},
		Plans:     &stubPlans{},
		Assigner:  &stubAssigner{},
		CSRFToken: func(*http.Request) string { return "" },
		Logger:    discardLogger(),
		Billing:   billing,
	}
	h, err := master.New(deps)
	if err != nil {
		t.Fatalf("master.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/billing"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// AC #3 — RLS isolation. Two complementary cases:
//
//   - the in-process stub plays the part of the WithTenant-scoped
//     runtime pool: when the request asks for tenant Y's billing but
//     the seed data belongs to tenant X, the result is zero rows;
//   - the handler-level defensive gate (crossTenantPermitted) makes
//     the non-master "cross-tenant" attempt visible as a 403 instead
//     of relying entirely on the wire layer's RequireAction.
func TestShowBilling_MasterCrossTenantSeesEmptyWhenStubFiltersOut(t *testing.T) {
	billing := &stubBilling{tenantID: acmeTenantID(), view: sampleBillingView()}
	_, mux := newC11Handler(t, billing, nil)

	// Master operator legitimately spans tenants — should reach the
	// adapter, which returns the empty view for the other tenant.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+otherTenantID().String()+"/billing"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Pro") || strings.Contains(body, "9aaaaaaa-0000-0000-0000-000000000001") {
		t.Errorf("cross-tenant leak: tenant X data appears in tenant Y response")
	}
	for _, want := range []string{
		"Tenant sem assinatura ativa",
		"Nenhum invoice emitido",
		"Nenhuma cortesia emitida",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected empty state %q in cross-tenant response", want)
		}
	}
}

func TestShowBilling_GerenteDifferentTenant403(t *testing.T) {
	// Gerente of tenant Y hits the URL for tenant X — the handler-level
	// cross-tenant gate MUST block before any port call so the leak path
	// is doubly closed (gate + RLS in the adapter).
	billing := &stubBilling{tenantID: acmeTenantID(), view: sampleBillingView()}
	_, mux := newC11Handler(t, billing, nil)

	rec := httptest.NewRecorder()
	req := reqWithGerenteC11(http.MethodGet,
		"/master/tenants/"+acmeTenantID().String()+"/billing",
		otherTenantID()) // principal is gerente of "other"; URL is "acme"
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if billing.calls != 0 {
		t.Errorf("port called %d times; expected 0 (gate should block first)", billing.calls)
	}
}

func TestShowBilling_GerenteSameTenantAllowed(t *testing.T) {
	// Same-tenant gerente: gate allows, port serves.
	billing := &stubBilling{tenantID: acmeTenantID(), view: sampleBillingView()}
	_, mux := newC11Handler(t, billing, nil)

	rec := httptest.NewRecorder()
	req := reqWithGerenteC11(http.MethodGet,
		"/master/tenants/"+acmeTenantID().String()+"/billing",
		acmeTenantID())
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-plan-slug="pro"`) {
		t.Errorf("same-tenant gerente should see the full panel")
	}
}

func TestShowBilling_AccessibleStructure(t *testing.T) {
	billing := &stubBilling{tenantID: acmeTenantID(), view: sampleBillingView()}
	_, mux := newC11Handler(t, billing, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/billing"))

	body := rec.Body.String()
	for _, want := range []string{
		`role="main"`,
		`aria-labelledby="master-billing-title"`,
		`aria-labelledby="billing-sub-title"`,
		`aria-labelledby="billing-inv-title"`,
		`aria-labelledby="billing-grants-title"`,
		`aria-label="Histórico de cobrança do tenant"`,
		`<caption class="visually-hidden">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing a11y marker %q", want)
		}
	}
}

// ----- ShowLedger ------------------------------------------------------

func TestShowLedger_RendersFirstPage(t *testing.T) {
	rows := ledgerFixture(75)
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: rows}
	_, mux := newC11Handler(t, nil, ledger)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/ledger"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<!doctype html>",
		`id="master-ledger-title"`,
		`id="ledger-panel"`,
		`id="ledger-rows"`,
		`data-ledger-source-label="master_grant"`,
		`data-ledger-source-label="monthly_alloc"`,
		`data-ledger-source-label="consumption"`,
		// At least one master-grant ref renders with the link
		`href="/master/tenants/` + acmeTenantID().String() + `/grants/new#grant-row-`,
		// load-more
		`id="ledger-load-more"`,
		`hx-target="#ledger-rows"`,
		`hx-swap="beforeend"`,
		// Default page size honored
		"cursor_at=",
		"cursor_id=",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestShowLedger_NoEntriesRendersEmpty(t *testing.T) {
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: nil}
	_, mux := newC11Handler(t, nil, ledger)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/ledger"))

	body := rec.Body.String()
	if !strings.Contains(body, "Nenhum lançamento encontrado") {
		t.Errorf("expected empty state; body=%s", body)
	}
	if strings.Contains(body, `id="ledger-load-more"`) {
		t.Errorf("empty result should NOT render load-more")
	}
}

func TestShowLedger_CursorRoundTrip(t *testing.T) {
	rows := ledgerFixture(120)
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: rows}
	_, mux := newC11Handler(t, nil, ledger)

	// First page with explicit small page size.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/ledger?page_size=10"))
	if rec.Code != http.StatusOK {
		t.Fatalf("first page status = %d", rec.Code)
	}
	if ledger.lastOpts.PageSize != 10 {
		t.Fatalf("PageSize = %d, want 10", ledger.lastOpts.PageSize)
	}
	if !ledger.lastOpts.CursorOccurredAt.IsZero() {
		t.Fatalf("first page CursorOccurredAt should be zero, got %v", ledger.lastOpts.CursorOccurredAt)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "cursor_at=") {
		t.Fatalf("first page missing cursor link; body=%s", body)
	}
	// Extract cursor_at/cursor_id from the body so we can do a real
	// second-page request.
	cursorAt, cursorID := extractCursor(t, body)

	// Second page with that cursor.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, reqWithMasterC11(http.MethodGet,
		"/master/tenants/"+acmeTenantID().String()+
			"/ledger?page_size=10&cursor_at="+cursorAt+"&cursor_id="+cursorID))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second page status = %d", rec2.Code)
	}
	if ledger.lastOpts.CursorOccurredAt.IsZero() {
		t.Errorf("second page should have parsed cursor")
	}
	if ledger.lastOpts.PageSize != 10 {
		t.Errorf("second page PageSize = %d, want 10", ledger.lastOpts.PageSize)
	}
	// New rows must NOT include rows already shown on page 1.
	firstPageIDs := extractRowIDs(t, body)
	secondBody := rec2.Body.String()
	for id := range firstPageIDs {
		if strings.Contains(secondBody, `data-ledger-id="`+id+`"`) {
			t.Errorf("page 2 leaked page-1 row id %s", id)
		}
	}
}

func TestShowLedger_PageSizeClamped(t *testing.T) {
	rows := ledgerFixture(50)
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: rows}
	_, mux := newC11Handler(t, nil, ledger)

	cases := []struct {
		name        string
		query       string
		wantClamped int
	}{
		// Oversize values fall back to the default rather than being
		// silently clamped to the maximum — same shape as the C9
		// tenants list (TestListTenants_ClampsBadQueryParams).
		{"oversize page_size", "?page_size=10000", 50},
		{"zero page_size", "?page_size=0", 50},
		{"negative page_size", "?page_size=-1", 50},
		{"non-numeric page_size", "?page_size=abc", 50},
		{"empty", "", 50},
		// At-cap value stays untouched.
		{"at-max page_size", "?page_size=200", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/ledger"+tc.query))
			if ledger.lastOpts.PageSize != tc.wantClamped {
				t.Errorf("PageSize = %d, want %d", ledger.lastOpts.PageSize, tc.wantClamped)
			}
		})
	}
}

func TestShowLedger_RendersLoadMoreWhenHasMore(t *testing.T) {
	rows := ledgerFixture(60)
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: rows}
	_, mux := newC11Handler(t, nil, ledger)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/ledger?page_size=10"))
	body := rec.Body.String()
	if !strings.Contains(body, `id="ledger-load-more"`) {
		t.Errorf("expected load-more row; body=%s", body)
	}
	if !strings.Contains(body, `Carregar mais`) {
		t.Errorf("expected load-more button label")
	}
}

func TestShowLedger_HXTargetRowsReturnsRowsOnly(t *testing.T) {
	rows := ledgerFixture(60)
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: rows}
	_, mux := newC11Handler(t, nil, ledger)

	req := reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/ledger?page_size=10")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "ledger-rows")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("rows-only partial should not include layout")
	}
	if strings.Contains(body, `id="ledger-panel"`) {
		t.Errorf("rows-only partial should not include panel container")
	}
	if !strings.Contains(body, `data-ledger-source-label="master_grant"`) {
		t.Errorf("rows-only partial should include ledger rows")
	}
}

func TestShowLedger_HXRequestWithoutTargetReturnsPanel(t *testing.T) {
	rows := ledgerFixture(60)
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: rows}
	_, mux := newC11Handler(t, nil, ledger)

	req := reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/ledger")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("HX-Request panel response should not include layout")
	}
	if !strings.Contains(body, `id="ledger-panel"`) {
		t.Errorf("HX-Request without HX-Target should include panel container")
	}
}

func TestShowLedger_InvalidUUID(t *testing.T) {
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: nil}
	_, mux := newC11Handler(t, nil, ledger)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/bad-id/ledger"))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestShowLedger_MalformedCursorFallsBackToFirstPage(t *testing.T) {
	rows := ledgerFixture(20)
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: rows}
	_, mux := newC11Handler(t, nil, ledger)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet,
		"/master/tenants/"+acmeTenantID().String()+"/ledger?cursor_at=not-a-time&cursor_id=not-a-uuid"))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (malformed cursor should not 4xx)", rec.Code)
	}
	if !ledger.lastOpts.CursorOccurredAt.IsZero() {
		t.Errorf("malformed cursor should fall back to zero, got %v", ledger.lastOpts.CursorOccurredAt)
	}
	if ledger.lastOpts.CursorID != uuid.Nil {
		t.Errorf("malformed cursor id should fall back to zero, got %v", ledger.lastOpts.CursorID)
	}
}

func TestShowLedger_PortError(t *testing.T) {
	ledger := &stubLedger{tenantID: acmeTenantID(), err: errors.New("kaboom")}
	_, mux := newC11Handler(t, nil, ledger)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/ledger"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestShowLedger_MissingPrincipalIs401(t *testing.T) {
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: ledgerFixture(10)}
	_, mux := newC11Handler(t, nil, ledger)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/ledger", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestShowLedger_PortNotConfiguredIs503(t *testing.T) {
	_, mux := newC11Handler(t, nil, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/ledger"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestShowLedger_EmptyCSRFIs500(t *testing.T) {
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: ledgerFixture(10)}
	deps := master.Deps{
		Tenants:   &stubLister{},
		Creator:   &stubCreator{},
		Plans:     &stubPlans{},
		Assigner:  &stubAssigner{},
		CSRFToken: func(*http.Request) string { return "" },
		Logger:    discardLogger(),
		Ledger:    ledger,
	}
	h, err := master.New(deps)
	if err != nil {
		t.Fatalf("master.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/ledger"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestShowLedger_MasterCrossTenantSeesEmptyWhenStubFiltersOut(t *testing.T) {
	// AC #3 — master operator legitimately spans tenants. The stub's
	// tenant filter mirrors the WithTenant-scoped runtime adapter's RLS
	// guarantee → empty page when reading another tenant's ledger via
	// the runtime pool.
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: ledgerFixture(50)}
	_, mux := newC11Handler(t, nil, ledger)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+otherTenantID().String()+"/ledger"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `data-ledger-source-label="master_grant"`) {
		t.Errorf("cross-tenant leak in ledger response")
	}
	if !strings.Contains(body, "Nenhum lançamento encontrado") {
		t.Errorf("expected empty state on cross-tenant ledger fetch")
	}
}

func TestShowLedger_GerenteDifferentTenant403(t *testing.T) {
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: ledgerFixture(20)}
	_, mux := newC11Handler(t, nil, ledger)

	rec := httptest.NewRecorder()
	req := reqWithGerenteC11(http.MethodGet,
		"/master/tenants/"+acmeTenantID().String()+"/ledger",
		otherTenantID())
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestShowLedger_GerenteSameTenantAllowed(t *testing.T) {
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: ledgerFixture(20)}
	_, mux := newC11Handler(t, nil, ledger)

	rec := httptest.NewRecorder()
	req := reqWithGerenteC11(http.MethodGet,
		"/master/tenants/"+acmeTenantID().String()+"/ledger",
		acmeTenantID())
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="ledger-panel"`) {
		t.Errorf("same-tenant gerente should see ledger panel")
	}
}

func TestShowLedger_AccessibleStructure(t *testing.T) {
	ledger := &stubLedger{tenantID: acmeTenantID(), rows: ledgerFixture(10)}
	_, mux := newC11Handler(t, nil, ledger)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMasterC11(http.MethodGet, "/master/tenants/"+acmeTenantID().String()+"/ledger"))

	body := rec.Body.String()
	for _, want := range []string{
		`role="main"`,
		`aria-labelledby="master-ledger-title"`,
		`aria-label="Ledger de tokens"`,
		`<caption class="visually-hidden">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing a11y marker %q", want)
		}
	}
}

// ----- Pure helpers (formatters) ---------------------------------------

func TestExportInt64ToStr(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{-1, "-1"},
		{1_000_000, "1000000"},
		{-1_000_000, "-1000000"},
	}
	for _, tc := range cases {
		if got := master.ExportInt64ToStr(tc.in); got != tc.want {
			t.Errorf("int64ToStr(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ----- Cursor-extraction helpers ---------------------------------------

// extractCursor pulls the cursor_at and cursor_id query params out of
// the load-more button's hx-get attribute in the rendered HTML. Avoids
// importing an HTML parser for what is a single regex-like scan.
func extractCursor(t *testing.T, body string) (string, string) {
	t.Helper()
	const prefix = "cursor_at="
	idx := strings.Index(body, prefix)
	if idx < 0 {
		t.Fatalf("body missing cursor_at marker")
	}
	rest := body[idx+len(prefix):]
	end := strings.IndexAny(rest, "&\"")
	if end < 0 {
		t.Fatalf("body missing cursor_at terminator")
	}
	cursorAt := rest[:end]
	const idPrefix = "cursor_id="
	idx2 := strings.Index(rest, idPrefix)
	if idx2 < 0 {
		t.Fatalf("body missing cursor_id marker")
	}
	rest2 := rest[idx2+len(idPrefix):]
	end2 := strings.IndexAny(rest2, "&\"")
	if end2 < 0 {
		t.Fatalf("body missing cursor_id terminator")
	}
	return cursorAt, rest2[:end2]
}

// extractRowIDs returns the set of data-ledger-id attributes present
// in the rendered body. Used to assert non-overlap between paginated
// responses.
func extractRowIDs(t *testing.T, body string) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	const marker = `data-ledger-id="`
	i := 0
	for {
		idx := strings.Index(body[i:], marker)
		if idx < 0 {
			break
		}
		start := i + idx + len(marker)
		end := strings.Index(body[start:], `"`)
		if end < 0 {
			break
		}
		out[body[start:start+end]] = struct{}{}
		i = start + end
	}
	return out
}
