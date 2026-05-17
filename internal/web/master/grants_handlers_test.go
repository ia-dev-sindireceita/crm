package master_test

// SIN-62884 / Fase 2.5 C10 — tests for the master grants HTMX surface.
// The unit tests cover the handler-edge contract: form validation,
// kind switch, cap enforcement, revoke gating on consumed_at,
// HTMX partial-swap shape, CSRF rendering, and 503 fail-fast when
// the grants port is not wired.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/web/master"
)

// ----- Fake GrantPort ---------------------------------------------------

type fakeGrants struct {
	mu sync.Mutex

	rows []master.GrantRow

	// Knobs the test can flip to inject behaviour.
	issueErr  error
	revokeErr error
	listErr   error

	// Capture of the last call (so we can assert inputs).
	lastIssue  master.IssueGrantInput
	lastRevoke master.RevokeGrantInput
	calls      int
}

func newFakeGrants() *fakeGrants {
	return &fakeGrants{}
}

func (f *fakeGrants) IssueGrant(_ context.Context, in master.IssueGrantInput) (master.IssueGrantResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastIssue = in
	if f.issueErr != nil {
		return master.IssueGrantResult{}, f.issueErr
	}
	row := master.GrantRow{
		ID:          uuid.New(),
		ExternalID:  fmt.Sprintf("01ULID%d", f.calls),
		TenantID:    in.TenantID,
		Kind:        in.Kind,
		PeriodDays:  in.PeriodDays,
		Amount:      in.Amount,
		Reason:      in.Reason,
		CreatedByID: in.ActorUserID,
		CreatedAt:   time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC),
	}
	f.rows = append([]master.GrantRow{row}, f.rows...)
	return master.IssueGrantResult{Grant: row}, nil
}

func (f *fakeGrants) RevokeGrant(_ context.Context, in master.RevokeGrantInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRevoke = in
	if f.revokeErr != nil {
		return f.revokeErr
	}
	for i, g := range f.rows {
		if g.ID == in.GrantID {
			if g.Consumed {
				return master.ErrGrantAlreadyConsumed
			}
			if g.Revoked {
				return master.ErrGrantAlreadyRevoked
			}
			g.Revoked = true
			g.RevokedAt = time.Date(2026, 5, 16, 13, 0, 0, 0, time.UTC)
			g.RevokeBy = in.ActorUserID
			f.rows[i] = g
			return nil
		}
	}
	return master.ErrGrantNotFound
}

func (f *fakeGrants) ListGrants(_ context.Context, _ uuid.UUID) ([]master.GrantRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]master.GrantRow, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

// ----- Fixtures ---------------------------------------------------------

const fakeTenantID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

func newGrantsHandler(t *testing.T, grants master.GrantPort) (*master.Handler, *fakeGrants) {
	t.Helper()
	var fg *fakeGrants
	if grants == nil {
		fg = newFakeGrants()
		grants = fg
	}
	deps := master.Deps{
		Tenants:   &stubLister{},
		Creator:   &stubCreator{},
		Plans:     &stubPlans{},
		Assigner:  &stubAssigner{},
		CSRFToken: func(*http.Request) string { return "csrf-test-token" },
		Logger:    discardLogger(),
		Grants:    grants,
	}
	h, err := master.New(deps)
	if err != nil {
		t.Fatalf("master.New: %v", err)
	}
	if fg == nil {
		return h, nil
	}
	return h, fg
}

func grantsMux(t *testing.T, h *master.Handler) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func issueFormBody(kind, days, amount, reason string) string {
	v := url.Values{}
	v.Set("kind", kind)
	if days != "" {
		v.Set("period_days", days)
	}
	if amount != "" {
		v.Set("amount", amount)
	}
	v.Set("reason", reason)
	return v.Encode()
}

func revokeFormBody(tenantID, reason string) string {
	v := url.Values{}
	v.Set("tenant_id", tenantID)
	v.Set("reason", reason)
	return v.Encode()
}

// ----- ShowGrantsForm ---------------------------------------------------

func TestShowGrantsForm_RendersFormAndEmptyList(t *testing.T) {
	h, _ := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/tenants/"+fakeTenantID+"/grants/new", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<!doctype html>",
		"id=\"master-grants-title\"",
		"id=\"grants-panel\"",
		"name=\"kind\"",
		"value=\"free_subscription_period\"",
		"value=\"extra_tokens\"",
		"name=\"period_days\"",
		"name=\"amount\"",
		"name=\"reason\"",
		"csrf-test-token",
		"hx-headers=",
		"Nenhuma cortesia emitida",
		"/master/tenants/" + fakeTenantID + "/grants",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestShowGrantsForm_HXRequestRendersPartial(t *testing.T) {
	h, _ := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	req := reqWithMaster(http.MethodGet, "/master/tenants/"+fakeTenantID+"/grants/new", "")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("HX-Request should return partial, got full layout")
	}
	if !strings.Contains(body, "id=\"grants-panel\"") {
		t.Errorf("partial missing #grants-panel")
	}
}

func TestShowGrantsForm_InvalidTenantID(t *testing.T) {
	h, _ := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/tenants/not-a-uuid/grants/new", ""))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ----- IssueGrant -------------------------------------------------------

func TestIssueGrant_FreeSubscriptionPeriod_SuccessRendersHistory(t *testing.T) {
	h, fg := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants",
		issueFormBody("free_subscription_period", "30", "", "concessao por incidente prod")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"id=\"grants-panel\"",
		"Cortesia concedida com sucesso.",
		"Período grátis",
		"30 dias",
		"concessao por incidente prod",
		"data-grant-state=\"active\"",
		"Revogar",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nfull=%s", want, body)
		}
	}
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("response should be partial, not full layout")
	}
	if got := fg.lastIssue.Kind; got != master.GrantKindFreeSubscriptionPeriod {
		t.Errorf("port called with kind=%q, want free_subscription_period", got)
	}
	if got := fg.lastIssue.PeriodDays; got != 30 {
		t.Errorf("port called with period_days=%d, want 30", got)
	}
	if got := fg.lastIssue.Amount; got != 0 {
		t.Errorf("port called with amount=%d for period grant, want 0", got)
	}
}

func TestIssueGrant_ExtraTokens_SuccessRendersHistory(t *testing.T) {
	h, fg := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants",
		issueFormBody("extra_tokens", "", "500000", "compensacao por outage do dia 16")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Tokens extras",
		"500000 tokens",
		"compensacao por outage do dia 16",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if got := fg.lastIssue.Amount; got != 500000 {
		t.Errorf("port called with amount=%d, want 500000", got)
	}
	if got := fg.lastIssue.PeriodDays; got != 0 {
		t.Errorf("port called with period_days=%d for token grant, want 0", got)
	}
}

func TestIssueGrant_ReasonTooShort(t *testing.T) {
	h, fg := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants",
		issueFormBody("extra_tokens", "", "1000", "curto")))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Motivo deve ter pelo menos 10 caracteres.") {
		t.Errorf("body missing reason-too-short error: %s", body)
	}
	if fg.calls != 0 {
		t.Errorf("port called %d times on validation failure, want 0", fg.calls)
	}
}

func TestIssueGrant_InvalidKind(t *testing.T) {
	h, _ := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants",
		issueFormBody("bogus_kind", "", "", "razao valida com mais de dez chars")))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Tipo de cortesia inválido.") {
		t.Errorf("body missing invalid-kind error: %s", rec.Body.String())
	}
}

func TestIssueGrant_NonPositivePeriodDays(t *testing.T) {
	h, _ := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants",
		issueFormBody("free_subscription_period", "0", "", "razao valida com mais de dez chars")))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Período em dias deve ser um inteiro positivo.") {
		t.Errorf("body missing period_days error: %s", rec.Body.String())
	}
}

func TestIssueGrant_PeriodDaysOverflow(t *testing.T) {
	h, _ := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants",
		issueFormBody("free_subscription_period", "500", "", "razao valida com mais de dez chars")))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Período em dias não pode exceder 366.") {
		t.Errorf("body missing overflow error: %s", rec.Body.String())
	}
}

func TestIssueGrant_NonPositiveAmount(t *testing.T) {
	h, _ := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants",
		issueFormBody("extra_tokens", "", "0", "razao valida com mais de dez chars")))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Quantidade de tokens deve ser um inteiro positivo.") {
		t.Errorf("body missing amount error: %s", rec.Body.String())
	}
}

func TestIssueGrant_PerGrantCapExceeded(t *testing.T) {
	fg := newFakeGrants()
	fg.issueErr = master.ErrPerGrantCapExceeded
	h, _ := newGrantsHandler(t, fg)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants",
		issueFormBody("extra_tokens", "", "20000000", "concessao acima do cap")))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Valor acima do limite por grant. Requer aprovação 4-eyes") {
		t.Errorf("body missing per-grant cap error: %s", rec.Body.String())
	}
}

func TestIssueGrant_PerTenantWindowCapExceeded(t *testing.T) {
	fg := newFakeGrants()
	fg.issueErr = master.ErrPerTenantWindowCapExceeded
	h, _ := newGrantsHandler(t, fg)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants",
		issueFormBody("extra_tokens", "", "8000000", "concessao no cap acumulado")))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Tenant excedeu o limite acumulado de 365 dias") {
		t.Errorf("body missing cumulative cap error: %s", rec.Body.String())
	}
}

func TestIssueGrant_PortErrorIs500(t *testing.T) {
	fg := newFakeGrants()
	fg.issueErr = fmt.Errorf("db down")
	h, _ := newGrantsHandler(t, fg)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/"+fakeTenantID+"/grants",
		issueFormBody("extra_tokens", "", "100", "concessao valida razao")))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestIssueGrant_InvalidTenantID(t *testing.T) {
	h, _ := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/tenants/not-a-uuid/grants",
		issueFormBody("extra_tokens", "", "100", "concessao valida razao")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ----- RevokeGrant ------------------------------------------------------

func TestRevokeGrant_Success(t *testing.T) {
	h, fg := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	// Seed one grant so revoke has something to act on.
	grantID := uuid.New()
	fg.rows = []master.GrantRow{{
		ID:        grantID,
		TenantID:  uuid.MustParse(fakeTenantID),
		Kind:      master.GrantKindExtraTokens,
		Amount:    1000,
		Reason:    "razao valida",
		CreatedAt: time.Date(2026, 5, 16, 11, 0, 0, 0, time.UTC),
	}}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/"+grantID.String()+"/revoke",
		revokeFormBody(fakeTenantID, "incidente identificado depois")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"id=\"grants-panel\"",
		"Grant revogado com sucesso.",
		"data-grant-state=\"revoked\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nfull=%s", want, body)
		}
	}
	if fg.lastRevoke.GrantID != grantID {
		t.Errorf("revoke called with id=%s, want %s", fg.lastRevoke.GrantID, grantID)
	}
}

func TestRevokeGrant_ConsumedGrantShowsCompensatoryNote(t *testing.T) {
	fg := newFakeGrants()
	grantID := uuid.New()
	fg.rows = []master.GrantRow{{
		ID:         grantID,
		TenantID:   uuid.MustParse(fakeTenantID),
		Kind:       master.GrantKindFreeSubscriptionPeriod,
		PeriodDays: 30,
		Reason:     "razao valida",
		Consumed:   true,
		ConsumedAt: time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC),
		CreatedAt:  time.Date(2026, 5, 16, 11, 0, 0, 0, time.UTC),
	}}
	h, _ := newGrantsHandler(t, fg)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/"+grantID.String()+"/revoke",
		revokeFormBody(fakeTenantID, "incidente identificado depois")))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Grant já consumido.",
		"emita uma cortesia compensatória",
		"data-grant-state=\"consumed\"",
		"Já consumida — emita uma compensatória",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nfull=%s", want, body)
		}
	}
}

func TestRevokeGrant_AlreadyRevokedReturns409(t *testing.T) {
	fg := newFakeGrants()
	fg.revokeErr = master.ErrGrantAlreadyRevoked
	h, _ := newGrantsHandler(t, fg)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/"+uuid.NewString()+"/revoke",
		revokeFormBody(fakeTenantID, "duplicate revoke attempt")))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestRevokeGrant_NotFoundReturns404(t *testing.T) {
	fg := newFakeGrants()
	fg.revokeErr = master.ErrGrantNotFound
	h, _ := newGrantsHandler(t, fg)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/"+uuid.NewString()+"/revoke",
		revokeFormBody(fakeTenantID, "razao valida bem longa")))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRevokeGrant_ReasonTooShort(t *testing.T) {
	h, fg := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/"+uuid.NewString()+"/revoke",
		revokeFormBody(fakeTenantID, "curto")))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Motivo da revogação deve ter pelo menos 10 caracteres.") {
		t.Errorf("body missing revoke reason error: %s", rec.Body.String())
	}
	if fg.lastRevoke.GrantID != uuid.Nil {
		t.Errorf("port called on validation failure: %v", fg.lastRevoke)
	}
}

func TestRevokeGrant_InvalidTenantID(t *testing.T) {
	h, _ := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/"+uuid.NewString()+"/revoke",
		revokeFormBody("not-a-uuid", "razao valida bem longa")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRevokeGrant_InvalidGrantID(t *testing.T) {
	h, _ := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodPost,
		"/master/grants/not-a-uuid/revoke",
		revokeFormBody(fakeTenantID, "razao valida bem longa")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ----- nil Grants port --------------------------------------------------

func TestGrantsHandlers_503WhenPortNotWired(t *testing.T) {
	// Construct handler without the Grants port — the tenants surface
	// should keep working but grants routes return 503.
	deps := master.Deps{
		Tenants:   &stubLister{},
		Creator:   &stubCreator{},
		Plans:     &stubPlans{},
		Assigner:  &stubAssigner{},
		CSRFToken: func(*http.Request) string { return "csrf-test-token" },
		Logger:    discardLogger(),
	}
	h, err := master.New(deps)
	if err != nil {
		t.Fatalf("master.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/master/tenants/" + fakeTenantID + "/grants/new", ""},
		{http.MethodPost, "/master/tenants/" + fakeTenantID + "/grants",
			issueFormBody("extra_tokens", "", "100", "razao valida")},
		{http.MethodPost, "/master/grants/" + uuid.NewString() + "/revoke",
			revokeFormBody(fakeTenantID, "razao valida")},
	}
	for _, tc := range cases {
		t.Run(tc.method+"_"+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithMaster(tc.method, tc.path, tc.body))
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// ----- Unauthenticated requests -----------------------------------------

func TestGrantsHandlers_401WhenNoPrincipal(t *testing.T) {
	h, _ := newGrantsHandler(t, nil)
	mux := grantsMux(t, h)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"show", http.MethodGet, "/master/tenants/" + fakeTenantID + "/grants/new", ""},
		{"issue", http.MethodPost, "/master/tenants/" + fakeTenantID + "/grants",
			issueFormBody("extra_tokens", "", "100", "razao valida")},
		{"revoke", http.MethodPost, "/master/grants/" + uuid.NewString() + "/revoke",
			revokeFormBody(fakeTenantID, "razao valida bem longa")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			var req *http.Request
			if tc.body == "" {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			} else {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			// No iam.WithPrincipal — handler should return 401.
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// ----- Empty CSRF token --------------------------------------------------

func TestGrantsHandlers_500WhenCSRFTokenEmpty(t *testing.T) {
	deps := master.Deps{
		Tenants:   &stubLister{},
		Creator:   &stubCreator{},
		Plans:     &stubPlans{},
		Assigner:  &stubAssigner{},
		CSRFToken: func(*http.Request) string { return "" },
		Logger:    discardLogger(),
		Grants:    newFakeGrants(),
	}
	h, err := master.New(deps)
	if err != nil {
		t.Fatalf("master.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithMaster(http.MethodGet, "/master/tenants/"+fakeTenantID+"/grants/new", ""))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// ----- principalLite proxy unused -- the principal extraction goes
// via iam.PrincipalFromContext already, no need to re-export.

// ----- iam check on principal handling ---------------------------------

// Compile-time assertion that the fake satisfies the port.
var _ master.GrantPort = (*fakeGrants)(nil)

// Sanity: iam package wires the master role for the principal.
func TestMasterPrincipalHasRoleMaster(t *testing.T) {
	p := masterPrincipal()
	found := false
	for _, r := range p.Roles {
		if r == iam.RoleMaster {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("masterPrincipal() missing RoleMaster: %v", p.Roles)
	}
}
