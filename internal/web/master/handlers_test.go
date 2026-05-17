package master_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/web/master"
)

// ----- Stubs ------------------------------------------------------------

type stubLister struct {
	res  master.ListResult
	err  error
	last master.ListOptions
}

func (s *stubLister) List(_ context.Context, opts master.ListOptions) (master.ListResult, error) {
	s.last = opts
	return s.res, s.err
}

type stubCreator struct {
	res  master.CreateTenantResult
	err  error
	last master.CreateTenantInput
}

func (s *stubCreator) Create(_ context.Context, in master.CreateTenantInput) (master.CreateTenantResult, error) {
	s.last = in
	return s.res, s.err
}

type stubPlans struct {
	plans []billing.Plan
	err   error
}

func (s *stubPlans) List(_ context.Context) ([]billing.Plan, error) {
	return s.plans, s.err
}

type stubAssigner struct {
	res  master.AssignPlanResult
	err  error
	last master.AssignPlanInput
}

func (s *stubAssigner) Assign(_ context.Context, in master.AssignPlanInput) (master.AssignPlanResult, error) {
	s.last = in
	return s.res, s.err
}

// ----- Fixtures ---------------------------------------------------------

func freePlan() billing.Plan {
	return billing.Plan{
		ID:                uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		Slug:              "free",
		Name:              "Free",
		PriceCentsBRL:     0,
		MonthlyTokenQuota: 1_000,
	}
}

func proPlan() billing.Plan {
	return billing.Plan{
		ID:                uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		Slug:              "pro",
		Name:              "Pro",
		PriceCentsBRL:     19_900,
		MonthlyTokenQuota: 1_000_000,
	}
}

func acmeRow() master.TenantRow {
	return master.TenantRow{
		ID:                   uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		Name:                 "Acme Cobranças",
		Host:                 "acme.crm.local",
		PlanSlug:             "pro",
		PlanName:             "Pro",
		SubscriptionStatus:   "active",
		LastInvoiceState:     "paid",
		LastInvoiceUpdatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
}

func masterPrincipal() iam.Principal {
	return iam.Principal{
		UserID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TenantID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Roles:    []iam.Role{iam.RoleMaster},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newHandler is the canonical fixture builder. Tests override fields
// via the optional Deps slot (only one field at a time in practice).
func newHandler(t *testing.T, overrides master.Deps) (*master.Handler, *stubLister, *stubCreator, *stubPlans, *stubAssigner) {
	t.Helper()
	lister := &stubLister{res: master.ListResult{Tenants: []master.TenantRow{acmeRow()}, Page: 1, PageSize: 25, TotalCount: 1}}
	creator := &stubCreator{res: master.CreateTenantResult{Tenant: acmeRow()}}
	plans := &stubPlans{plans: []billing.Plan{freePlan(), proPlan()}}
	assigner := &stubAssigner{res: master.AssignPlanResult{Tenant: acmeRow()}}
	deps := master.Deps{
		Tenants:   lister,
		Creator:   creator,
		Plans:     plans,
		Assigner:  assigner,
		CSRFToken: func(*http.Request) string { return "csrf-test-token" },
		Logger:    discardLogger(),
	}
	if overrides.Tenants != nil {
		deps.Tenants = overrides.Tenants
	}
	if overrides.Creator != nil {
		deps.Creator = overrides.Creator
	}
	if overrides.Plans != nil {
		deps.Plans = overrides.Plans
	}
	if overrides.Assigner != nil {
		deps.Assigner = overrides.Assigner
	}
	if overrides.CSRFToken != nil {
		deps.CSRFToken = overrides.CSRFToken
	}
	if overrides.DefaultPageSize > 0 {
		deps.DefaultPageSize = overrides.DefaultPageSize
	}
	if overrides.MaxPageSize > 0 {
		deps.MaxPageSize = overrides.MaxPageSize
	}
	h, err := master.New(deps)
	if err != nil {
		t.Fatalf("master.New: %v", err)
	}
	return h, lister, creator, plans, assigner
}

func newMux(t *testing.T, h *master.Handler) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func reqWithMaster(method, target string, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return r.WithContext(iam.WithPrincipal(r.Context(), masterPrincipal()))
}

// ----- New (constructor) ------------------------------------------------

func TestNew_RejectsMissingDeps(t *testing.T) {
	base := master.Deps{
		Tenants:   &stubLister{},
		Creator:   &stubCreator{},
		Plans:     &stubPlans{},
		Assigner:  &stubAssigner{},
		CSRFToken: func(*http.Request) string { return "x" },
	}
	cases := []struct {
		name   string
		mutate func(d *master.Deps)
	}{
		{"missing tenants", func(d *master.Deps) { d.Tenants = nil }},
		{"missing creator", func(d *master.Deps) { d.Creator = nil }},
		{"missing plans", func(d *master.Deps) { d.Plans = nil }},
		{"missing assigner", func(d *master.Deps) { d.Assigner = nil }},
		{"missing csrf", func(d *master.Deps) { d.CSRFToken = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := base
			tc.mutate(&d)
			if _, err := master.New(d); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

func TestNew_RejectsInvalidPageSizeConfig(t *testing.T) {
	d := master.Deps{
		Tenants:         &stubLister{},
		Creator:         &stubCreator{},
		Plans:           &stubPlans{},
		Assigner:        &stubAssigner{},
		CSRFToken:       func(*http.Request) string { return "x" },
		DefaultPageSize: 200,
		MaxPageSize:     50,
	}
	if _, err := master.New(d); err == nil {
		t.Fatalf("expected DefaultPageSize > MaxPageSize to fail")
	}
}

func TestNew_DefaultsLogger(t *testing.T) {
	h, err := master.New(master.Deps{
		Tenants:   &stubLister{},
		Creator:   &stubCreator{},
		Plans:     &stubPlans{},
		Assigner:  &stubAssigner{},
		CSRFToken: func(*http.Request) string { return "x" },
	})
	if err != nil || h == nil {
		t.Fatalf("New default logger path: h=%v err=%v", h, err)
	}
}

// ----- ListTenants ------------------------------------------------------

func TestListTenants_RendersFullPage(t *testing.T) {
	h, _, _, _, _ := newHandler(t, master.Deps{})
	mux := newMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/tenants", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<!doctype html>",
		"id=\"master-tenants-title\"",
		"Acme Cobranças",
		"acme.crm.local",
		"id=\"tenants-table\"",
		"id=\"tenant-name\"",
		"id=\"tenant-host\"",
		"id=\"tenant-plan\"",
		"id=\"tenant-courtesy\"",
		"csrf-test-token", // CSRF meta + hx-headers + form input
		"hx-headers=",
		"name=\"_csrf\"",
		// Plans render inside the select
		"value=\"pro\"",
		"value=\"free\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
}

func TestListTenants_HXRequestRendersPartial(t *testing.T) {
	h, _, _, _, _ := newHandler(t, master.Deps{})
	mux := newMux(t, h)

	req := reqWithMaster(http.MethodGet, "/master/tenants?page=1&plan=pro", "")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("HX-Request response should be partial, got full layout")
	}
	if !strings.Contains(body, "id=\"tenants-table\"") {
		t.Errorf("partial missing #tenants-table")
	}
}

func TestListTenants_AppliesQueryParams(t *testing.T) {
	h, lister, _, _, _ := newHandler(t, master.Deps{})
	mux := newMux(t, h)

	req := reqWithMaster(http.MethodGet, "/master/tenants?page=3&page_size=10&plan=pro", "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if lister.last.Page != 3 {
		t.Errorf("Page = %d, want 3", lister.last.Page)
	}
	if lister.last.PageSize != 10 {
		t.Errorf("PageSize = %d, want 10", lister.last.PageSize)
	}
	if lister.last.FilterPlanSlug != "pro" {
		t.Errorf("FilterPlanSlug = %q, want pro", lister.last.FilterPlanSlug)
	}
}

func TestListTenants_ClampsBadQueryParams(t *testing.T) {
	h, lister, _, _, _ := newHandler(t, master.Deps{DefaultPageSize: 25, MaxPageSize: 50})
	mux := newMux(t, h)

	cases := []struct {
		name         string
		query        string
		wantPage     int
		wantPageSize int
	}{
		{"negative page", "?page=-3&page_size=10", 1, 10},
		{"bad page", "?page=abc&page_size=10", 1, 10},
		{"oversize page_size", "?page=2&page_size=1000", 2, 25},
		{"zero page_size", "?page=2&page_size=0", 2, 25},
		{"empty", "", 1, 25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := reqWithMaster(http.MethodGet, "/master/tenants"+tc.query, "")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if lister.last.Page != tc.wantPage {
				t.Errorf("Page = %d, want %d", lister.last.Page, tc.wantPage)
			}
			if lister.last.PageSize != tc.wantPageSize {
				t.Errorf("PageSize = %d, want %d", lister.last.PageSize, tc.wantPageSize)
			}
		})
	}
}

func TestListTenants_EmptyResultRendersEmptyRow(t *testing.T) {
	lister := &stubLister{res: master.ListResult{Tenants: nil, TotalCount: 0}}
	h, _, _, _, _ := newHandler(t, master.Deps{Tenants: lister})
	mux := newMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/tenants", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Nenhum tenant encontrado") {
		t.Errorf("body should announce empty state")
	}
}

func TestListTenants_ListerError(t *testing.T) {
	lister := &stubLister{err: errors.New("kaboom")}
	h, _, _, _, _ := newHandler(t, master.Deps{Tenants: lister})
	mux := newMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/tenants", ""))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestListTenants_ListerNotFoundRendersEmpty(t *testing.T) {
	lister := &stubLister{err: master.ErrNotFound}
	h, _, _, _, _ := newHandler(t, master.Deps{Tenants: lister})
	mux := newMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/tenants", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Nenhum tenant encontrado") {
		t.Errorf("ErrNotFound from list should render empty state")
	}
}

func TestListTenants_PlanListerError(t *testing.T) {
	plans := &stubPlans{err: errors.New("plans-down")}
	h, _, _, _, _ := newHandler(t, master.Deps{Plans: plans})
	mux := newMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/tenants", ""))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestListTenants_RejectsMissingPrincipal(t *testing.T) {
	h, _, _, _, _ := newHandler(t, master.Deps{})
	mux := newMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/master/tenants", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestListTenants_EmptyCSRFTokenIs500(t *testing.T) {
	h, _, _, _, _ := newHandler(t, master.Deps{
		CSRFToken: func(*http.Request) string { return "" },
	})
	mux := newMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/tenants", ""))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// ----- CreateTenant -----------------------------------------------------

func TestCreateTenant_HappyPath(t *testing.T) {
	h, _, creator, _, _ := newHandler(t, master.Deps{})
	mux := newMux(t, h)

	body := url.Values{
		"name":            {"Acme Cobranças"},
		"host":            {"acme.crm.local"},
		"plan_slug":       {"pro"},
		"courtesy_tokens": {"500"},
	}.Encode()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost, "/master/tenants", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if creator.last.Name != "Acme Cobranças" {
		t.Errorf("Name = %q, want Acme Cobranças", creator.last.Name)
	}
	if creator.last.Host != "acme.crm.local" {
		t.Errorf("Host = %q", creator.last.Host)
	}
	if creator.last.PlanSlug != "pro" {
		t.Errorf("PlanSlug = %q", creator.last.PlanSlug)
	}
	if creator.last.InitialCourtesyTokens != 500 {
		t.Errorf("InitialCourtesyTokens = %d", creator.last.InitialCourtesyTokens)
	}
	if creator.last.ActorUserID != masterPrincipal().UserID {
		t.Errorf("ActorUserID = %s", creator.last.ActorUserID)
	}
	body2 := rec.Body.String()
	if !strings.Contains(body2, "Tenant criado com sucesso") {
		t.Errorf("missing success flash")
	}
	if !strings.Contains(body2, "Acme Cobranças") {
		t.Errorf("created row not rendered")
	}
}

func TestCreateTenant_EnsuresRowVisibleEvenIfListerStale(t *testing.T) {
	// Lister returns nothing but the new row should still appear.
	lister := &stubLister{res: master.ListResult{Tenants: nil, TotalCount: 0}}
	created := master.TenantRow{
		ID:   uuid.New(),
		Name: "FreshlyCreated",
		Host: "fresh.local",
	}
	creator := &stubCreator{res: master.CreateTenantResult{Tenant: created}}
	h, _, _, _, _ := newHandler(t, master.Deps{Tenants: lister, Creator: creator})
	mux := newMux(t, h)

	body := url.Values{"name": {"x"}, "host": {"fresh.local"}}.Encode()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost, "/master/tenants", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "FreshlyCreated") {
		t.Errorf("freshly-created row not rendered after stale lister read")
	}
}

func TestCreateTenant_ValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		form    url.Values
		wantMsg string
	}{
		{"missing name", url.Values{"host": {"a.local"}}, "Nome do tenant é obrigatório"},
		{"missing host", url.Values{"name": {"X"}}, "Host do tenant é obrigatório"},
		{"bad host", url.Values{"name": {"X"}, "host": {"a b"}}, "Host inválido"},
		{"negative courtesy", url.Values{"name": {"X"}, "host": {"a.local"}, "courtesy_tokens": {"-1"}}, "Tokens de cortesia"},
		{"non-numeric courtesy", url.Values{"name": {"X"}, "host": {"a.local"}, "courtesy_tokens": {"x"}}, "Tokens de cortesia"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _, _ := newHandler(t, master.Deps{})
			mux := newMux(t, h)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithMaster(http.MethodPost, "/master/tenants", tc.form.Encode()))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.wantMsg) {
				t.Errorf("body missing %q; got=%s", tc.wantMsg, rec.Body.String())
			}
		})
	}
}

func TestCreateTenant_PortErrors(t *testing.T) {
	cases := []struct {
		name     string
		creator  *stubCreator
		wantCode int
		wantMsg  string
	}{
		{"host taken", &stubCreator{err: master.ErrHostTaken}, http.StatusUnprocessableEntity, "host já está em uso"},
		{"unknown plan", &stubCreator{err: master.ErrUnknownPlan}, http.StatusUnprocessableEntity, "Plano desconhecido"},
		{"invalid input", &stubCreator{err: master.ErrInvalidInput}, http.StatusUnprocessableEntity, "inválidos"},
		{"internal", &stubCreator{err: errors.New("kaboom")}, http.StatusInternalServerError, "Internal Server Error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _, _ := newHandler(t, master.Deps{Creator: tc.creator})
			mux := newMux(t, h)
			body := url.Values{"name": {"X"}, "host": {"x.local"}}.Encode()
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithMaster(http.MethodPost, "/master/tenants", body))
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if !strings.Contains(rec.Body.String(), tc.wantMsg) {
				t.Errorf("body missing %q; got=%s", tc.wantMsg, rec.Body.String())
			}
		})
	}
}

func TestCreateTenant_RejectsMissingPrincipal(t *testing.T) {
	h, _, _, _, _ := newHandler(t, master.Deps{})
	mux := newMux(t, h)

	rec := httptest.NewRecorder()
	body := url.Values{"name": {"X"}, "host": {"x.local"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/master/tenants", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// ----- AssignPlan -------------------------------------------------------

func TestAssignPlan_HappyPath(t *testing.T) {
	h, _, _, _, assigner := newHandler(t, master.Deps{})
	mux := newMux(t, h)

	tid := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	body := url.Values{"plan_slug": {"pro"}}.Encode()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPatch, "/master/tenants/"+tid.String()+"/plan", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if assigner.last.TenantID != tid {
		t.Errorf("TenantID = %s, want %s", assigner.last.TenantID, tid)
	}
	if assigner.last.PlanSlug != "pro" {
		t.Errorf("PlanSlug = %q", assigner.last.PlanSlug)
	}
	if assigner.last.ActorUserID != masterPrincipal().UserID {
		t.Errorf("ActorUserID = %s", assigner.last.ActorUserID)
	}
	resp := rec.Body.String()
	if !strings.Contains(resp, "id=\"tenant-row-"+tid.String()+"\"") {
		t.Errorf("row partial missing tenant-row id")
	}
	if !strings.Contains(resp, "hx-patch=") {
		t.Errorf("row partial should keep hx-patch form for next swap")
	}
}

func TestAssignPlan_InvalidUUID(t *testing.T) {
	h, _, _, _, _ := newHandler(t, master.Deps{})
	mux := newMux(t, h)

	rec := httptest.NewRecorder()
	body := url.Values{"plan_slug": {"pro"}}.Encode()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPatch, "/master/tenants/not-a-uuid/plan", body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAssignPlan_MissingPlanSlug(t *testing.T) {
	h, _, _, _, _ := newHandler(t, master.Deps{})
	mux := newMux(t, h)

	tid := uuid.New()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPatch, "/master/tenants/"+tid.String()+"/plan", ""))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestAssignPlan_PortErrors(t *testing.T) {
	cases := []struct {
		name     string
		assigner *stubAssigner
		wantCode int
	}{
		{"not found", &stubAssigner{err: master.ErrNotFound}, http.StatusNotFound},
		{"unknown plan", &stubAssigner{err: master.ErrUnknownPlan}, http.StatusUnprocessableEntity},
		{"internal", &stubAssigner{err: errors.New("kaboom")}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _, _ := newHandler(t, master.Deps{Assigner: tc.assigner})
			mux := newMux(t, h)
			tid := uuid.New()
			body := url.Values{"plan_slug": {"pro"}}.Encode()
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithMaster(http.MethodPatch, "/master/tenants/"+tid.String()+"/plan", body))
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
		})
	}
}

func TestAssignPlan_RejectsMissingPrincipal(t *testing.T) {
	h, _, _, _, _ := newHandler(t, master.Deps{})
	mux := newMux(t, h)

	tid := uuid.New()
	body := url.Values{"plan_slug": {"pro"}}.Encode()
	req := httptest.NewRequest(http.MethodPatch, "/master/tenants/"+tid.String()+"/plan", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAssignPlan_PlanListerFailureStillRendersRow(t *testing.T) {
	plans := &stubPlans{err: errors.New("transient")}
	h, _, _, _, _ := newHandler(t, master.Deps{Plans: plans})
	mux := newMux(t, h)

	tid := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	body := url.Values{"plan_slug": {"pro"}}.Encode()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPatch, "/master/tenants/"+tid.String()+"/plan", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), tid.String()) {
		t.Errorf("row partial missing tenant id")
	}
}

func TestAssignPlan_EmptyCSRFTokenIs500(t *testing.T) {
	h, _, _, _, _ := newHandler(t, master.Deps{
		CSRFToken: func(*http.Request) string { return "" },
	})
	mux := newMux(t, h)
	tid := uuid.New()
	body := url.Values{"plan_slug": {"pro"}}.Encode()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPatch, "/master/tenants/"+tid.String()+"/plan", body))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// ----- Accessibility / form structure -----------------------------------

func TestListTenants_AccessibleStructure(t *testing.T) {
	h, _, _, _, _ := newHandler(t, master.Deps{})
	mux := newMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/tenants", ""))

	body := rec.Body.String()
	for _, want := range []string{
		`role="main"`,
		`aria-labelledby="master-tenants-title"`,
		`<label for="tenant-name">`,
		`<label for="tenant-host">`,
		`<label for="tenant-plan">`,
		`<label for="tenant-courtesy">`,
		`aria-label="Filtrar por plano"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing a11y marker %q", want)
		}
	}
}

// ----- ListResult totals & pagination -----------------------------------

func TestListTenants_PaginationLinks(t *testing.T) {
	lister := &stubLister{res: master.ListResult{
		Tenants:    []master.TenantRow{acmeRow()},
		Page:       2,
		PageSize:   25,
		TotalCount: 60,
	}}
	h, _, _, _, _ := newHandler(t, master.Deps{Tenants: lister})
	mux := newMux(t, h)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/tenants?page=2", ""))

	body := rec.Body.String()
	if !strings.Contains(body, "Página 2 de 3") {
		t.Errorf("pagination caption missing; body=%s", body)
	}
	if !strings.Contains(body, "Página anterior") || !strings.Contains(body, "Próxima página") {
		t.Errorf("pagination nav missing")
	}
}
