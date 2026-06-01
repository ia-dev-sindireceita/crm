package walletui_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/web/walletui"
)

// ---------------------------------------------------------------------------
// stubs
// ---------------------------------------------------------------------------

type stubDashboard struct {
	mu        sync.Mutex
	in        uuid.UUID
	calls     int
	snapshot  walletui.DashboardSnapshot
	err       error
	gotNow    time.Time
	returnErr error
}

func (s *stubDashboard) Snapshot(_ context.Context, tenantID uuid.UUID, now time.Time) (walletui.DashboardSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = tenantID
	s.gotNow = now
	s.calls++
	if s.returnErr != nil {
		return walletui.DashboardSnapshot{}, s.returnErr
	}
	return s.snapshot, s.err
}

type stubLedger struct {
	mu        sync.Mutex
	in        walletui.LedgerPageOptions
	called    bool
	page      walletui.LedgerPage
	err       error
	csvFilter walletui.LedgerFilter
	csvCalls  int
	csvRows   [][]string
	csvErr    error
}

func (s *stubLedger) Page(_ context.Context, opts walletui.LedgerPageOptions) (walletui.LedgerPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = opts
	s.called = true
	return s.page, s.err
}

func (s *stubLedger) StreamCSV(_ context.Context, f walletui.LedgerFilter, w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.csvFilter = f
	s.csvCalls++
	if s.csvErr != nil {
		return s.csvErr
	}
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"id", "occurred_at", "kind", "source", "amount", "conversation_id_hash", "model", "policy_id", "external_ref"}); err != nil {
		return err
	}
	for _, row := range s.csvRows {
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

type stubTopup struct {
	mu       sync.Mutex
	packages []walletui.TopupPackage
	err      error
	calls    int
}

func (s *stubTopup) ListPackages(_ context.Context) ([]walletui.TopupPackage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.packages, s.err
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newHandler(t *testing.T, dash walletui.DashboardReader, ledger walletui.LedgerReader, topup walletui.TopupCatalogReader) *walletui.Handler {
	t.Helper()
	h, err := walletui.New(walletui.Deps{
		Dashboard: dash,
		Ledger:    ledger,
		Topup:     topup,
		CSRFToken: func(*http.Request) string { return "csrf-test-token" },
		Now:       func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("walletui.New: %v", err)
	}
	return h
}

func reqWithTenant(method, target string, tenantID uuid.UUID, hxRequest bool) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	tenant := &tenancy.Tenant{ID: tenantID, Name: "Acme"}
	r = r.WithContext(tenancy.WithContext(r.Context(), tenant))
	if hxRequest {
		r.Header.Set("HX-Request", "true")
	}
	return r
}

// ---------------------------------------------------------------------------
// New() — required deps
// ---------------------------------------------------------------------------

func TestNew_RejectsMissingRequiredDeps(t *testing.T) {
	t.Parallel()
	full := walletui.Deps{
		Dashboard: &stubDashboard{},
		Ledger:    &stubLedger{},
		Topup:     &stubTopup{},
		CSRFToken: func(*http.Request) string { return "tok" },
	}
	if _, err := walletui.New(full); err != nil {
		t.Fatalf("full deps: unexpected error %v", err)
	}
	cases := map[string]walletui.Deps{
		"no dashboard": {Ledger: full.Ledger, Topup: full.Topup, CSRFToken: full.CSRFToken},
		"no ledger":    {Dashboard: full.Dashboard, Topup: full.Topup, CSRFToken: full.CSRFToken},
		"no topup":     {Dashboard: full.Dashboard, Ledger: full.Ledger, CSRFToken: full.CSRFToken},
		"no csrf":      {Dashboard: full.Dashboard, Ledger: full.Ledger, Topup: full.Topup},
	}
	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := walletui.New(deps); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Dashboard handler
// ---------------------------------------------------------------------------

func TestDashboard_RendersOkBalance(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	last := walletui.LedgerEntryView{
		ID:           uuid.New(),
		OccurredAt:   time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC),
		Kind:         wallet.KindCommit,
		Source:       wallet.SourceConsumption,
		Amount:       -250,
		BalanceAfter: 12_000,
		Model:        "openrouter/gpt-4o-mini",
	}
	days := 30
	dash := &stubDashboard{snapshot: walletui.DashboardSnapshot{
		Balance:         12_500,
		Reserved:        500,
		Available:       12_000,
		AvgDailyConsume: 400,
		DaysRemaining:   &days,
		LastFive:        []walletui.LedgerEntryView{last},
	}}
	h := newHandler(t, dash, &stubLedger{}, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet", tenant, false))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if dash.in != tenant {
		t.Errorf("tenant: got %s want %s", dash.in, tenant)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="wallet-dashboard"`,
		`data-testid="wallet-balance-card"`,
		`data-severity="ok"`,
		`Saldo confortável`,
		`12.000`,  // available
		`400`,     // avg daily consume (humanInt)
		`30 dias`, // days remaining
		`Últimos lançamentos`,
		`Consumo confirmado`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if cache := rec.Header().Get("Cache-Control"); cache != "private, max-age=30" {
		t.Errorf("Cache-Control: got %q want %q", cache, "private, max-age=30")
	}
}

func TestDashboard_SeverityLadder(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	avg := int64(400)
	monthly := avg * 30
	tt := []struct {
		name     string
		snap     walletui.DashboardSnapshot
		wantSev  string
		wantText string
	}{
		{
			name: "warn at 19% available",
			snap: walletui.DashboardSnapshot{
				Balance: monthly * 19 / 100, Available: monthly * 19 / 100, AvgDailyConsume: avg,
			},
			wantSev:  "warn",
			wantText: "Saldo baixo",
		},
		{
			name: "critical at 4% available",
			snap: walletui.DashboardSnapshot{
				Balance: monthly * 4 / 100, Available: monthly * 4 / 100, AvgDailyConsume: avg,
			},
			wantSev:  "critical",
			wantText: "Saldo crítico",
		},
		{
			name: "blocked when dunning suspended_outbound",
			snap: walletui.DashboardSnapshot{
				Balance: 1_000_000, Available: 1_000_000, AvgDailyConsume: avg,
				DunningState: "suspended_outbound",
			},
			wantSev:  "blocked",
			wantText: "Saldo bloqueado",
		},
		{
			name: "blocked when dunning suspended_full",
			snap: walletui.DashboardSnapshot{
				Balance: 1_000_000, Available: 1_000_000,
				DunningState: "suspended_full",
			},
			wantSev:  "blocked",
			wantText: "Saldo bloqueado",
		},
		{
			name: "ok with no consumption history",
			snap: walletui.DashboardSnapshot{
				Balance: 1_000, Available: 1_000, AvgDailyConsume: 0,
			},
			wantSev:  "ok",
			wantText: "Consumo ainda não registrado",
		},
	}
	for _, tc := range tt {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dash := &stubDashboard{snapshot: tc.snap}
			h := newHandler(t, dash, &stubLedger{}, &stubTopup{})
			mux := http.NewServeMux()
			h.Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet", tenant, false))
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d want 200", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `data-severity="`+tc.wantSev+`"`) {
				t.Errorf("severity attr missing %q\nbody=%s", tc.wantSev, body)
			}
			if !strings.Contains(body, tc.wantText) {
				t.Errorf("severity label missing %q", tc.wantText)
			}
		})
	}
}

func TestDashboard_RendersDunningBanners(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	override := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		snap        walletui.DashboardSnapshot
		bannerTest  string
		wantVisible bool
	}{
		{"warn", walletui.DashboardSnapshot{DunningState: "warn"}, "wallet-banner-warn", true},
		{"suspended_outbound", walletui.DashboardSnapshot{DunningState: "suspended_outbound"}, "wallet-banner-outbound", true},
		{"suspended_full", walletui.DashboardSnapshot{DunningState: "suspended_full"}, "wallet-banner-full", true},
		{"override", walletui.DashboardSnapshot{DunningOverrideUntil: &override}, "wallet-banner-override", true},
		{"clean", walletui.DashboardSnapshot{}, "wallet-banner-", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dash := &stubDashboard{snapshot: tc.snap}
			h := newHandler(t, dash, &stubLedger{}, &stubTopup{})
			mux := http.NewServeMux()
			h.Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet", tenant, false))
			body := rec.Body.String()
			if tc.wantVisible {
				if !strings.Contains(body, `data-testid="`+tc.bannerTest+`"`) {
					t.Errorf("expected banner %q in body", tc.bannerTest)
				}
			} else {
				if strings.Contains(body, `data-testid="`+tc.bannerTest) {
					t.Errorf("unexpected banner in clean snapshot")
				}
			}
		})
	}
}

func TestDashboard_TolerantOfTenantNotFound(t *testing.T) {
	t.Parallel()
	dash := &stubDashboard{returnErr: walletui.ErrTenantNotFound}
	h := newHandler(t, dash, &stubLedger{}, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet", uuid.New(), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; tenant-not-found should render empty dashboard", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `Saldo confortável`) {
		t.Errorf("expected default ok severity when no snapshot")
	}
}

func TestDashboard_FailsWhenTenantMissing(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubDashboard{}, &stubLedger{}, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/wallet", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestDashboard_FailsWhenDashboardErrors(t *testing.T) {
	t.Parallel()
	dash := &stubDashboard{returnErr: errors.New("boom")}
	h := newHandler(t, dash, &stubLedger{}, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet", uuid.New(), false))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestDashboard_FailsWhenCSRFTokenEmpty(t *testing.T) {
	t.Parallel()
	dash := &stubDashboard{}
	h, err := walletui.New(walletui.Deps{
		Dashboard: dash,
		Ledger:    &stubLedger{},
		Topup:     &stubTopup{},
		CSRFToken: func(*http.Request) string { return "" },
	})
	if err != nil {
		t.Fatalf("walletui.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet", uuid.New(), false))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Topup handler
// ---------------------------------------------------------------------------

func TestTopup_RendersBestValueBadge(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	topup := &stubTopup{packages: []walletui.TopupPackage{
		{ID: uuid.New(), Slug: "starter", Name: "Starter", Tokens: 100_000, PriceCentsBRL: 9900, PricePerKToken: 99},
		{ID: uuid.New(), Slug: "pro", Name: "Pro", Tokens: 500_000, PriceCentsBRL: 39900, PricePerKToken: 80},
	}}
	h := newHandler(t, &stubDashboard{}, &stubLedger{}, topup)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet/topup", tenant, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="wallet-topup"`,
		`Starter`,
		`Pro`,
		`wallet-topup-card--best`,
		`Melhor custo`,
		`R$ 99,00`,
		`R$ 399,00`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Best-value should be on the Pro card (cheaper per-k-token).
	// Verify the best-value badge sits on the pro card by checking
	// the slug and the badge label both appear between the two card
	// boundaries.
	if !strings.Contains(body, `aria-label="Pro — melhor custo por mil tokens"`) {
		t.Errorf("best-value aria-label missing on pro card; body=%s", body)
	}
}

func TestTopup_RendersEmptyState(t *testing.T) {
	t.Parallel()
	topup := &stubTopup{packages: nil}
	h := newHandler(t, &stubDashboard{}, &stubLedger{}, topup)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet/topup", uuid.New(), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `data-testid="wallet-topup-empty"`) {
		t.Errorf("empty state missing")
	}
}

func TestTopup_FailsWhenListErrors(t *testing.T) {
	t.Parallel()
	topup := &stubTopup{err: errors.New("boom")}
	h := newHandler(t, &stubDashboard{}, &stubLedger{}, topup)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet/topup", uuid.New(), false))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Ledger handler
// ---------------------------------------------------------------------------

func TestLedger_RendersFullPage(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	entryID := uuid.New()
	policyID := uuid.New()
	ledger := &stubLedger{page: walletui.LedgerPage{
		Entries: []walletui.LedgerEntryView{
			{
				ID:                 entryID,
				OccurredAt:         time.Date(2026, 5, 30, 9, 0, 0, 0, time.UTC),
				Kind:               wallet.KindCommit,
				Source:             wallet.SourceConsumption,
				Amount:             -100,
				BalanceAfter:       10_000,
				ConversationIDHash: "abcdef1234567890",
				Model:              "openrouter/x",
				PolicyID:           policyID,
			},
		},
		HasMore:              true,
		NextCursorOccurredAt: time.Date(2026, 5, 30, 8, 0, 0, 0, time.UTC),
		NextCursorID:         uuid.New(),
	}}
	h := newHandler(t, &stubDashboard{}, ledger, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet/ledger?kind=commit&from=2026-05-01&to=2026-05-31", tenant, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="wallet-ledger"`,
		`data-testid="wallet-ledger-row"`,
		`Consumo confirmado`,
		`conv abcdef12`,                   // first 8 chars of hash
		`policy ` + policyID.String()[:8], // policy chip
		`Carregar mais`,
		`wallet-csv-link`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Filter was forwarded to the adapter.
	if ledger.in.Filter.TenantID != tenant {
		t.Errorf("tenant: got %s want %s", ledger.in.Filter.TenantID, tenant)
	}
	if len(ledger.in.Filter.Kinds) != 1 || ledger.in.Filter.Kinds[0] != wallet.KindCommit {
		t.Errorf("kind filter: got %v", ledger.in.Filter.Kinds)
	}
	if ledger.in.Filter.FromOccurredAt.IsZero() {
		t.Errorf("from filter should be set")
	}
	if ledger.in.Filter.ToOccurredAt.IsZero() {
		t.Errorf("to filter should be set")
	}
}

func TestLedger_HXRequestReturnsRowsPartialOnly(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	ledger := &stubLedger{page: walletui.LedgerPage{
		Entries: []walletui.LedgerEntryView{
			{ID: uuid.New(), Kind: wallet.KindGrant, Source: wallet.SourceMonthlyAlloc, Amount: 1_000, OccurredAt: time.Now()},
		},
	}}
	h := newHandler(t, &stubDashboard{}, ledger, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet/ledger", tenant, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	body := rec.Body.String()
	// Partial only — no full shell.
	if strings.Contains(body, `<!doctype html>`) {
		t.Errorf("HX-Request response should be a partial, not full document")
	}
	if !strings.Contains(body, `id="wallet-ledger-rows"`) {
		t.Errorf("body missing rows wrapper id")
	}
}

func TestLedger_EmptyPageRendersEmptyState(t *testing.T) {
	t.Parallel()
	ledger := &stubLedger{page: walletui.LedgerPage{}}
	h := newHandler(t, &stubDashboard{}, ledger, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet/ledger", uuid.New(), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `wallet-ledger-empty`) {
		t.Errorf("empty state missing")
	}
}

func TestLedger_RejectsInvalidQueryParamsSafely(t *testing.T) {
	t.Parallel()
	ledger := &stubLedger{}
	h := newHandler(t, &stubDashboard{}, ledger, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	// Bogus cursor + kind + dates.
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet/ledger?cursor_at=not-a-time&cursor_id=not-uuid&kind=bogus&from=nope&to=nope&page_size=abc", uuid.New(), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("bogus params should collapse to defaults; status %d", rec.Code)
	}
	if !ledger.called {
		t.Fatalf("ledger.Page should still run with default options")
	}
	if ledger.in.PageSize == 0 {
		t.Fatalf("default page size must be applied")
	}
	if len(ledger.in.Filter.Kinds) != 0 {
		t.Errorf("invalid kind should drop to 'all'")
	}
}

func TestLedger_PageSizeClampedToMax(t *testing.T) {
	t.Parallel()
	ledger := &stubLedger{}
	h := newHandler(t, &stubDashboard{}, ledger, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet/ledger?page_size=999999", uuid.New(), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if ledger.in.PageSize <= 0 || ledger.in.PageSize > 200 {
		t.Fatalf("page size not clamped: %d", ledger.in.PageSize)
	}
}

func TestLedger_TolerantOfTenantNotFound(t *testing.T) {
	t.Parallel()
	ledger := &stubLedger{err: walletui.ErrTenantNotFound}
	h := newHandler(t, &stubDashboard{}, ledger, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet/ledger", uuid.New(), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; ErrTenantNotFound should render empty", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `wallet-ledger-empty`) {
		t.Errorf("expected empty state")
	}
}

func TestLedger_FailsOnAdapterError(t *testing.T) {
	t.Parallel()
	ledger := &stubLedger{err: errors.New("boom")}
	h := newHandler(t, &stubDashboard{}, ledger, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet/ledger", uuid.New(), false))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// CSV export
// ---------------------------------------------------------------------------

func TestLedgerCSV_StreamsHeaderAndRows(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	ledger := &stubLedger{csvRows: [][]string{
		{uuid.NewString(), "2026-05-30T10:00:00Z", "commit", "consumption", "-100", "abc", "model-x", "", "wamid-1"},
	}}
	h := newHandler(t, &stubDashboard{}, ledger, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/wallet/ledger.csv?kind=commit", tenant, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content-type: %q", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "wallet-ledger-2026-06-01") {
		t.Errorf("Content-Disposition: %q", cd)
	}
	if ledger.csvCalls != 1 {
		t.Errorf("StreamCSV called %d times", ledger.csvCalls)
	}
	if ledger.csvFilter.TenantID != tenant {
		t.Errorf("filter tenant: %s want %s", ledger.csvFilter.TenantID, tenant)
	}
	if len(ledger.csvFilter.Kinds) != 1 || ledger.csvFilter.Kinds[0] != wallet.KindCommit {
		t.Errorf("filter kinds: %v", ledger.csvFilter.Kinds)
	}
	// Parse CSV — header + 1 row.
	rd := csv.NewReader(bytes.NewReader(rec.Body.Bytes()))
	rows, err := rd.ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: got %d want 2", len(rows))
	}
	if rows[0][0] != "id" {
		t.Errorf("header[0]: %q", rows[0][0])
	}
}

func TestLedgerCSV_FailsWhenTenantMissing(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubDashboard{}, &stubLedger{}, &stubTopup{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/wallet/ledger.csv", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}
