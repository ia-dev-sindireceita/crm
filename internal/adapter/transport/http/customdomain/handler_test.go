package customdomain_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	cd "github.com/pericles-luz/crm/internal/adapter/transport/http/customdomain"
	"github.com/pericles-luz/crm/internal/customdomain/management"
)

// fakeUseCase is the test double for management.UseCase. Each call is
// scripted via the maps; missing scripts return zero values.
type fakeUseCase struct {
	mu          sync.Mutex
	listResp    []management.Domain
	listErr     error
	getResp     map[uuid.UUID]management.Domain
	getErr      map[uuid.UUID]error
	enrollResp  management.EnrollResult
	enrollErr   error
	verifyResp  management.VerifyOutcome
	verifyErr   error
	pauseResp   management.Domain
	pauseErr    error
	deleteErr   error
	regenResp   management.Domain
	regenErr    error
	enrollCalls []enrollCall
}

type enrollCall struct {
	tenant uuid.UUID
	host   string
}

func (f *fakeUseCase) List(_ context.Context, _ uuid.UUID) ([]management.Domain, error) {
	return f.listResp, f.listErr
}
func (f *fakeUseCase) Get(_ context.Context, _ uuid.UUID, id uuid.UUID) (management.Domain, error) {
	if e, ok := f.getErr[id]; ok && e != nil {
		return management.Domain{}, e
	}
	if d, ok := f.getResp[id]; ok {
		return d, nil
	}
	return management.Domain{}, management.ErrStoreNotFound
}
func (f *fakeUseCase) Enroll(_ context.Context, tenant uuid.UUID, host string) (management.EnrollResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrollCalls = append(f.enrollCalls, enrollCall{tenant: tenant, host: host})
	return f.enrollResp, f.enrollErr
}
func (f *fakeUseCase) Verify(_ context.Context, _ uuid.UUID, _ uuid.UUID) (management.VerifyOutcome, error) {
	return f.verifyResp, f.verifyErr
}
func (f *fakeUseCase) SetPaused(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ bool) (management.Domain, error) {
	return f.pauseResp, f.pauseErr
}
func (f *fakeUseCase) Delete(_ context.Context, _ uuid.UUID, _ uuid.UUID) error { return f.deleteErr }
func (f *fakeUseCase) RegenerateToken(_ context.Context, _ uuid.UUID, _ uuid.UUID) (management.Domain, error) {
	return f.regenResp, f.regenErr
}

const testCSRFSecret = "0123456789abcdef0123456789abcdef" // 32 bytes

var testTenant = uuid.New()

func newHandlerForTest(t *testing.T, uc *fakeUseCase) *cd.Handler {
	t.Helper()
	h, err := cd.New(cd.Config{
		UseCase:       uc,
		CSRF:          cd.CSRFConfig{Secret: []byte(testCSRFSecret)},
		PrimaryDomain: "exemplo.com",
		Now:           func() time.Time { return time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func newServeMux(h *cd.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

func withTenant(req *http.Request, tenantID uuid.UUID) *http.Request {
	return req.WithContext(cd.WithTenantID(req.Context(), tenantID))
}

func TestNew_ValidatesConfig(t *testing.T) {
	t.Parallel()
	if _, err := cd.New(cd.Config{}); err == nil {
		t.Fatal("expected error when UseCase is nil")
	}
	if _, err := cd.New(cd.Config{UseCase: &fakeUseCase{}}); err == nil {
		t.Fatal("expected error when secret too short")
	}
	if _, err := cd.New(cd.Config{UseCase: &fakeUseCase{}, CSRF: cd.CSRFConfig{Secret: []byte(testCSRFSecret)}}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestServeList_RequiresTenant(t *testing.T) {
	t.Parallel()
	h := newHandlerForTest(t, &fakeUseCase{})
	mux := newServeMux(h)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tenant/custom-domains", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestServeList_RendersDomains(t *testing.T) {
	t.Parallel()
	verified := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	domainID := uuid.New()
	uc := &fakeUseCase{
		listResp: []management.Domain{
			{ID: domainID, TenantID: testTenant, Host: "shop.example.com",
				VerifiedAt: &verified, VerifiedWithDNSSEC: true,
				CreatedAt: verified, UpdatedAt: verified},
		},
	}
	h := newHandlerForTest(t, uc)
	mux := newServeMux(h)
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains", nil), testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "shop.example.com") {
		t.Fatalf("body missing host: %s", body[:minInt(len(body), 500)])
	}
	if !strings.Contains(body, "Verificado") {
		t.Fatalf("missing PT-BR status badge")
	}
	if !strings.Contains(body, "DNSSEC") {
		t.Fatalf("missing DNSSEC indicator")
	}
	if !strings.Contains(body, `aria-label="Adicionar novo domínio personalizado"`) {
		t.Fatalf("missing aria-label on add button")
	}
	if !strings.Contains(body, "static.exemplo.com") {
		t.Fatalf("missing primary-domain helper text")
	}
	if rec.Result().Cookies() == nil || rec.Result().Cookies()[0].Name != cd.CSRFCookieName {
		t.Fatalf("missing CSRF cookie")
	}
}

func TestServeList_ListError(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{listErr: errors.New("pg down")}
	h := newHandlerForTest(t, uc)
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains", nil), testTenant)
	newServeMux(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeWizardStep1(t *testing.T) {
	t.Parallel()
	h := newHandlerForTest(t, &fakeUseCase{})
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/new", nil), testTenant)
	newServeMux(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Passo 1") {
		t.Fatalf("missing step header: %s", body)
	}
	if !strings.Contains(body, `name="_csrf"`) {
		t.Fatalf("missing CSRF input")
	}
}

// formPostWithCSRF performs the ritual of:
//
//	GET /tenant/custom-domains  → captures the CSRF cookie + token
//	POST <path>                  → sends both back per the double-submit pattern
//
// Returns the response recorder for assertions.
func formPostWithCSRF(t *testing.T, mux http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	primer := httptest.NewRecorder()
	primerReq := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/new", nil), testTenant)
	mux.ServeHTTP(primer, primerReq)
	cookies := primer.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("primer did not set CSRF cookie")
	}
	token := cookies[0].Value

	form := url.Values{}
	for _, kv := range strings.Split(body, "&") {
		if kv == "" {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		key := parts[0]
		val := ""
		if len(parts) == 2 {
			val = parts[1]
		}
		form.Add(key, val)
	}
	form.Set("_csrf", token)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookies[0])
	req = withTenant(req, testTenant)
	mux.ServeHTTP(rec, req)
	return rec
}

func TestServeEnroll_CSRFRejected(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tenant/custom-domains", strings.NewReader("host=shop.example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = withTenant(req, testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(uc.enrollCalls) != 0 {
		t.Fatal("Enroll was called despite missing CSRF")
	}
}

func TestServeEnroll_Step2Rendered(t *testing.T) {
	t.Parallel()
	domainID := uuid.New()
	uc := &fakeUseCase{
		enrollResp: management.EnrollResult{
			Domain:    management.Domain{ID: domainID, Host: "shop.example.com"},
			TXTRecord: "_crm-verify.shop.example.com",
			TXTValue:  "crm-verify=tok",
		},
	}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := formPostWithCSRF(t, mux, "/tenant/custom-domains", "host=shop.example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "_crm-verify.shop.example.com") {
		t.Fatalf("missing TXT record name")
	}
	if !strings.Contains(body, "crm-verify=tok") {
		t.Fatalf("missing TXT value")
	}
	if !strings.Contains(body, `aria-label="Copiar nome do registro TXT"`) {
		t.Fatalf("missing copy aria-label")
	}
	if len(uc.enrollCalls) != 1 || uc.enrollCalls[0].host != "shop.example.com" {
		t.Fatalf("Enroll calls = %+v", uc.enrollCalls)
	}
}

func TestServeEnroll_InvalidHost(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{enrollErr: management.ErrInvalidHost}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := formPostWithCSRF(t, mux, "/tenant/custom-domains", "host=127.0.0.1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "FQDN") {
		t.Fatalf("missing PT-BR error: %s", rec.Body.String())
	}
}

// TestServeEnroll_ErrorPreservesCSRF guards the regression CTO flagged on
// PR #41: the wizard-error template must keep a usable CSRF token so a
// retry without reloading the page is accepted by VerifyCSRF.
func TestServeEnroll_ErrorPreservesCSRF(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{enrollErr: management.ErrInvalidHost}
	h := newHandlerForTest(t, uc)
	mux := newServeMux(h)

	primer := httptest.NewRecorder()
	primerReq := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/new", nil), testTenant)
	mux.ServeHTTP(primer, primerReq)
	cookie := primer.Result().Cookies()[0]
	token := cookie.Value

	form := url.Values{}
	form.Set("host", "127.0.0.1")
	form.Set("_csrf", token)
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/tenant/custom-domains", strings.NewReader(form.Encode()))
	req1.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req1.AddCookie(cookie)
	req1 = withTenant(req1, testTenant)
	mux.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusUnprocessableEntity {
		t.Fatalf("first POST status = %d", rec1.Code)
	}
	body := rec1.Body.String()
	if !strings.Contains(body, `value="`+token+`"`) {
		t.Fatalf("error response stripped CSRF token; expected to see %q in form: %s", token, body)
	}
	if strings.Contains(body, `value=""`) {
		t.Fatalf("error response wrote an empty CSRF token: %s", body)
	}

	// Resubmit using the same cookie+token. The handler must accept the
	// CSRF check; the use-case still rejects (same scripted error) so we
	// re-receive 422, NOT 403.
	uc.enrollErr = management.ErrInvalidHost
	form2 := url.Values{}
	form2.Set("host", "shop.example.com")
	form2.Set("_csrf", token)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/tenant/custom-domains", strings.NewReader(form2.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.AddCookie(cookie)
	req2 = withTenant(req2, testTenant)
	mux.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusForbidden {
		t.Fatalf("resubmit was rejected by CSRF (403) instead of being processed")
	}
	if rec2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("resubmit status = %d, want 422", rec2.Code)
	}
}

func TestServeEnroll_PrivateIPCopy(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{enrollErr: management.ErrPrivateIP}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := formPostWithCSRF(t, mux, "/tenant/custom-domains", "host=internal.local")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "IP privado") {
		t.Fatalf("missing PT-BR private-ip copy: %s", rec.Body.String())
	}
}

func TestServeEnroll_RateLimitedRendersRetry(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{
		enrollResp: management.EnrollResult{Reason: management.ReasonRateLimited, RetryAfter: 17 * time.Minute},
	}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := formPostWithCSRF(t, mux, "/tenant/custom-domains", "host=shop.example.com")
	body := rec.Body.String()
	if !strings.Contains(body, "Limite de domínios cadastrados por hora atingido") {
		t.Fatalf("missing PT-BR rate-limit copy: %s", body)
	}
	if !strings.Contains(body, "17 minutos") {
		t.Fatalf("missing retry-after minutes: %s", body)
	}
}

func TestServeEnroll_ServerError(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{enrollErr: errors.New("boom")}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := formPostWithCSRF(t, mux, "/tenant/custom-domains", "host=shop.example.com")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeInstructions_RendersStep2(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	uc := &fakeUseCase{
		getResp: map[uuid.UUID]management.Domain{
			id: {ID: id, TenantID: testTenant, Host: "shop.example.com", VerificationToken: "tok"},
		},
	}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/"+id.String()+"/instructions", nil), testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "_crm-verify.shop.example.com") {
		t.Fatalf("missing TXT record")
	}
}

func TestServeInstructions_BadID(t *testing.T) {
	t.Parallel()
	mux := newServeMux(newHandlerForTest(t, &fakeUseCase{}))
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/not-a-uuid/instructions", nil), testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeInstructions_NotFound(t *testing.T) {
	t.Parallel()
	mux := newServeMux(newHandlerForTest(t, &fakeUseCase{}))
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/"+uuid.New().String()+"/instructions", nil), testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeStatusRow_RendersRow(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	uc := &fakeUseCase{
		getResp: map[uuid.UUID]management.Domain{
			id: {ID: id, TenantID: testTenant, Host: "shop.example.com"},
		},
	}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/"+id.String()+"/status", nil), testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="domain-row-`+id.String()+`"`) {
		t.Fatalf("row id missing: %s", body)
	}
	if !strings.Contains(body, `hx-trigger="every 30s"`) {
		t.Fatalf("polling trigger missing for pending row: %s", body)
	}
}

func TestServeDeleteModal(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	uc := &fakeUseCase{
		getResp: map[uuid.UUID]management.Domain{
			id: {ID: id, TenantID: testTenant, Host: "shop.example.com"},
		},
	}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/"+id.String()+"/delete", nil), testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `role="dialog"`) || !strings.Contains(body, `aria-modal="true"`) {
		t.Fatalf("modal missing accessibility attrs")
	}
	if !strings.Contains(body, "shop.example.com") {
		t.Fatalf("missing host in modal")
	}
	if !strings.Contains(body, "12 meses") {
		t.Fatalf("missing reservation lock copy")
	}
}

// htmxRequest issues a state-changing request (POST/PATCH/DELETE) using
// the X-CSRF-Token header path that the templates use.
func htmxRequest(t *testing.T, mux http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	primer := httptest.NewRecorder()
	primerReq := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains", nil), testTenant)
	mux.ServeHTTP(primer, primerReq)
	cookies := primer.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("primer did not set CSRF cookie")
	}
	tok := cookies[0].Value
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set(cd.CSRFHeader, tok)
	req.AddCookie(cookies[0])
	req = withTenant(req, testTenant)
	mux.ServeHTTP(rec, req)
	return rec
}

func TestServeVerify_RendersUpdatedRow(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	verified := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	uc := &fakeUseCase{
		verifyResp: management.VerifyOutcome{
			Verified: true,
			Domain:   management.Domain{ID: id, Host: "shop.example.com", VerifiedAt: &verified},
		},
	}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodPost, "/api/customdomains/"+id.String()+"/verify")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Verificado") {
		t.Fatalf("status badge missing: %s", rec.Body.String())
	}
}

func TestServeVerify_BadCSRF(t *testing.T) {
	t.Parallel()
	mux := newServeMux(newHandlerForTest(t, &fakeUseCase{}))
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodPost, "/api/customdomains/"+uuid.New().String()+"/verify", nil), testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeVerify_BadID(t *testing.T) {
	t.Parallel()
	mux := newServeMux(newHandlerForTest(t, &fakeUseCase{}))
	rec := htmxRequest(t, mux, http.MethodPost, "/api/customdomains/not-a-uuid/verify")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestServeVerify_FlipsToVerifiedWithDNSSEC is the SIN-62313 acceptance
// criterion at the handler boundary: when the management.UseCase
// returns a verified outcome with VerifiedWithDNSSEC=true (i.e. the
// SIN-62242 resolver matched the TXT and saw the AD bit), the rendered
// row carries the "Verificado" badge AND the DNSSEC indicator. The
// existing TestServeVerify_RendersUpdatedRow only checked the badge;
// this new case adds the DNSSEC assertion explicitly.
func TestServeVerify_FlipsToVerifiedWithDNSSEC(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	verified := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	uc := &fakeUseCase{
		verifyResp: management.VerifyOutcome{
			Verified: true,
			Domain: management.Domain{
				ID: id, Host: "shop.example.com",
				VerifiedAt: &verified, VerifiedWithDNSSEC: true,
			},
		},
	}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodPost, "/api/customdomains/"+id.String()+"/verify")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Verificado") {
		t.Fatalf("expected verified badge: %s", body)
	}
	if !strings.Contains(body, "DNSSEC") {
		t.Fatalf("expected DNSSEC indicator when VerifiedWithDNSSEC=true: %s", body)
	}
}

// TestServeVerify_FlipsToVerifiedWithoutDNSSEC mirrors
// FlipsToVerifiedWithDNSSEC but asserts the DNSSEC badge is omitted
// when the resolver did not see the AD bit. The pair locks down both
// halves of the `verified_with_dnssec` contract from SIN-62313.
func TestServeVerify_FlipsToVerifiedWithoutDNSSEC(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	verified := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	uc := &fakeUseCase{
		verifyResp: management.VerifyOutcome{
			Verified: true,
			Domain: management.Domain{
				ID: id, Host: "shop.example.com",
				VerifiedAt: &verified, VerifiedWithDNSSEC: false,
			},
		},
	}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodPost, "/api/customdomains/"+id.String()+"/verify")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Verificado") {
		t.Fatalf("expected verified badge: %s", body)
	}
	if strings.Contains(body, "DNSSEC") {
		t.Fatalf("DNSSEC indicator must be absent when VerifiedWithDNSSEC=false: %s", body)
	}
}

func TestServeVerify_TokenMismatchRendersErrorRow(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	uc := &fakeUseCase{
		verifyResp: management.VerifyOutcome{Domain: management.Domain{ID: id, Host: "shop.example.com"}, Reason: management.ReasonTokenMismatch, Err: management.ErrTokenMismatch},
		verifyErr:  management.ErrTokenMismatch,
	}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodPost, "/api/customdomains/"+id.String()+"/verify")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Pendente") && !strings.Contains(rec.Body.String(), "Erro") {
		t.Fatalf("expected status indicator: %s", rec.Body.String())
	}
}

func TestServeSetPaused_PauseAndResume(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	pausedAt := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	uc := &fakeUseCase{pauseResp: management.Domain{ID: id, Host: "shop.example.com", TLSPausedAt: &pausedAt}}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodPatch, "/api/customdomains/"+id.String()+"?paused=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Pausado") {
		t.Fatalf("missing paused badge: %s", rec.Body.String())
	}
}

func TestServeSetPaused_MissingFlag(t *testing.T) {
	t.Parallel()
	mux := newServeMux(newHandlerForTest(t, &fakeUseCase{}))
	rec := htmxRequest(t, mux, http.MethodPatch, "/api/customdomains/"+uuid.New().String())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeSetPaused_NotFound(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{pauseErr: management.ErrStoreNotFound}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodPatch, "/api/customdomains/"+uuid.New().String()+"?paused=true")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeSetPaused_ServerError(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{pauseErr: errors.New("boom")}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodPatch, "/api/customdomains/"+uuid.New().String()+"?paused=true")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestServeRegenerateToken_HappyPath verifies the HTTP contract for the
// success branch: 200, the response carries the row template with the
// new domain ID, and the use-case is invoked.
func TestServeRegenerateToken_HappyPath(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	issuedAt := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	uc := &fakeUseCase{
		regenResp: management.Domain{
			ID:                id,
			TenantID:          testTenant,
			Host:              "shop.example.com",
			VerificationToken: "freshly-rotated",
			TokenIssuedAt:     issuedAt,
			CreatedAt:         issuedAt,
			UpdatedAt:         issuedAt,
		},
	}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodPost, "/api/customdomains/"+id.String()+"/regenerate-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="domain-row-`+id.String()+`"`) {
		t.Fatalf("response should render the row partial with the domain id: %s", body[:minInt(len(body), 500)])
	}
	// Pending row must still expose the regenerate-token button so the
	// tenant can repeat the action if the new TXT also fails.
	if !strings.Contains(body, "Gerar novo token") {
		t.Fatalf("pending row missing regenerate-token button: %s", body[:minInt(len(body), 500)])
	}
	// The fresh token must NOT be reflected in the row template (the row
	// only shows status; the token is only ever rendered via the
	// instructions wizard).
	if strings.Contains(body, "freshly-rotated") {
		t.Fatalf("response leaks the verification token in the row partial: %s", body)
	}
}

// TestServeRegenerateToken_BadCSRF guards the double-submit gate: a POST
// without the X-CSRF-Token header (or matching cookie) must be rejected
// with 403, and the use-case must NOT be invoked.
func TestServeRegenerateToken_BadCSRF(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	uc := &fakeUseCase{
		regenResp: management.Domain{ID: id, TenantID: testTenant, Host: "shop.example.com"},
		regenErr:  errors.New("use-case must not be called"),
	}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodPost, "/api/customdomains/"+id.String()+"/regenerate-token", nil), testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// TestServeRegenerateToken_AlreadyVerified verifies that the
// ErrAlreadyVerified branch surfaces as 409 with the PT-BR copy so HTMX
// can swap the message into the row without re-rendering the page.
func TestServeRegenerateToken_AlreadyVerified(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{regenErr: management.ErrAlreadyVerified}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodPost, "/api/customdomains/"+uuid.New().String()+"/regenerate-token")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "verificado") {
		t.Fatalf("missing PT-BR already-verified copy: %s", rec.Body.String())
	}
}

// TestServeRegenerateToken_NotFound verifies the ErrStoreNotFound /
// ErrTenantMismatch branch returns 404 — the latter avoids the
// information-leakage signal a 403 would carry between tenants.
func TestServeRegenerateToken_NotFound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"store_not_found", management.ErrStoreNotFound},
		{"tenant_mismatch", management.ErrTenantMismatch},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			uc := &fakeUseCase{regenErr: tc.err}
			mux := newServeMux(newHandlerForTest(t, uc))
			rec := htmxRequest(t, mux, http.MethodPost, "/api/customdomains/"+uuid.New().String()+"/regenerate-token")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
		})
	}
}

// TestServeRegenerateToken_BadID guards the path-parsing branch: a
// non-UUID id must return 400 before the use-case is touched.
func TestServeRegenerateToken_BadID(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{regenErr: errors.New("use-case must not be called")}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodPost, "/api/customdomains/not-a-uuid/regenerate-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestServeRegenerateToken_RequiresTenant locks the tenant-required gate
// at the handler boundary. Without the tenant context (the dev-only
// middleware in cmd/server/customdomain_wire.go skips setting it when
// X-Tenant-ID is absent), the handler must short-circuit with 401
// before touching the CSRF check or the use-case.
func TestServeRegenerateToken_RequiresTenant(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{regenErr: errors.New("use-case must not be called")}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/customdomains/"+uuid.New().String()+"/regenerate-token", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestServeRegenerateToken_ServerError verifies that an unexpected
// use-case error from RegenerateToken is rendered as 500 — not leaked
// as the underlying error string, and not mis-classified as 4xx.
func TestServeRegenerateToken_ServerError(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{regenErr: errors.New("rotation pg blew up")}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodPost, "/api/customdomains/"+uuid.New().String()+"/regenerate-token")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "rotation pg blew up") {
		t.Fatalf("error body leaks internal error: %s", rec.Body.String())
	}
}

func TestServeDelete_ReListsTable(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	uc := &fakeUseCase{listResp: nil}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodDelete, "/api/customdomains/"+id.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="domain-list"`) {
		t.Fatalf("response should re-render domain-list: %s", rec.Body.String())
	}
}

func TestServeDelete_NotFound(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{deleteErr: management.ErrStoreNotFound}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodDelete, "/api/customdomains/"+uuid.New().String())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeDelete_DeleteError(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{deleteErr: errors.New("boom")}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodDelete, "/api/customdomains/"+uuid.New().String())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeDelete_ListErrorAfterDelete(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{listErr: errors.New("post-delete list failed")}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := htmxRequest(t, mux, http.MethodDelete, "/api/customdomains/"+uuid.New().String())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeDelete_BadID(t *testing.T) {
	t.Parallel()
	mux := newServeMux(newHandlerForTest(t, &fakeUseCase{}))
	rec := htmxRequest(t, mux, http.MethodDelete, "/api/customdomains/not-a-uuid")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeStatusRow_BadID(t *testing.T) {
	t.Parallel()
	mux := newServeMux(newHandlerForTest(t, &fakeUseCase{}))
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/not-a-uuid/status", nil), testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeStatusRow_NotFound(t *testing.T) {
	t.Parallel()
	mux := newServeMux(newHandlerForTest(t, &fakeUseCase{}))
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/"+uuid.New().String()+"/status", nil), testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeDeleteModal_NotFound(t *testing.T) {
	t.Parallel()
	mux := newServeMux(newHandlerForTest(t, &fakeUseCase{}))
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/"+uuid.New().String()+"/delete", nil), testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeDeleteModal_BadID(t *testing.T) {
	t.Parallel()
	mux := newServeMux(newHandlerForTest(t, &fakeUseCase{}))
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/not-a-uuid/delete", nil), testTenant)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestVerifyCSRFInvalidCookieValue(t *testing.T) {
	t.Parallel()
	cfg := cd.CSRFConfig{Secret: []byte(testCSRFSecret)}
	if err := cd.VerifyCSRFCookieValue("nope-no-dot", cfg.Secret); !errors.Is(err, cd.ErrCSRFInvalid) {
		t.Fatalf("expected ErrCSRFInvalid, got %v", err)
	}
	if err := cd.VerifyCSRFCookieValue("aGVsbG8.bm9wZQ", cfg.Secret); !errors.Is(err, cd.ErrCSRFInvalid) {
		t.Fatalf("expected ErrCSRFInvalid for bad mac, got %v", err)
	}
}

func TestVerifyCSRF_HeaderAndForm(t *testing.T) {
	t.Parallel()
	cfg := cd.CSRFConfig{Secret: []byte(testCSRFSecret)}
	wRec := httptest.NewRecorder()
	tok, err := cd.IssueCSRFToken(wRec, httptest.NewRequest(http.MethodGet, "/", nil), cfg)
	if err != nil {
		t.Fatalf("IssueCSRFToken: %v", err)
	}
	cookie := wRec.Result().Cookies()[0]

	// header path
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(cookie)
	req.Header.Set(cd.CSRFHeader, tok)
	if err := cd.VerifyCSRF(req, cfg); err != nil {
		t.Fatalf("header path: %v", err)
	}
	// form path
	req2 := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("_csrf="+tok))
	req2.AddCookie(cookie)
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := cd.VerifyCSRF(req2, cfg); err != nil {
		t.Fatalf("form path: %v", err)
	}
	// missing token both places
	req3 := httptest.NewRequest(http.MethodPost, "/", nil)
	req3.AddCookie(cookie)
	if err := cd.VerifyCSRF(req3, cfg); !errors.Is(err, cd.ErrCSRFInvalid) {
		t.Fatalf("expected ErrCSRFInvalid")
	}
	// no cookie at all
	if err := cd.VerifyCSRF(httptest.NewRequest(http.MethodPost, "/", nil), cfg); !errors.Is(err, cd.ErrCSRFInvalid) {
		t.Fatalf("expected ErrCSRFInvalid (no cookie)")
	}
	// mismatch
	req4 := httptest.NewRequest(http.MethodPost, "/", nil)
	req4.AddCookie(cookie)
	req4.Header.Set(cd.CSRFHeader, "wrong")
	if err := cd.VerifyCSRF(req4, cfg); !errors.Is(err, cd.ErrCSRFInvalid) {
		t.Fatalf("expected ErrCSRFInvalid (mismatch)")
	}
}

func TestIssueCSRFTokenReusesValid(t *testing.T) {
	t.Parallel()
	cfg := cd.CSRFConfig{Secret: []byte(testCSRFSecret)}
	w1 := httptest.NewRecorder()
	tok1, err := cd.IssueCSRFToken(w1, httptest.NewRequest(http.MethodGet, "/", nil), cfg)
	if err != nil {
		t.Fatalf("IssueCSRFToken: %v", err)
	}
	cookie := w1.Result().Cookies()[0]
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	w2 := httptest.NewRecorder()
	tok2, err := cd.IssueCSRFToken(w2, req, cfg)
	if err != nil {
		t.Fatalf("IssueCSRFToken second: %v", err)
	}
	if tok1 != tok2 {
		t.Fatalf("expected reuse, got %q vs %q", tok1, tok2)
	}
}

func TestTenantIDFromContext(t *testing.T) {
	t.Parallel()
	if got := cd.TenantIDFromContext(context.Background()); got != uuid.Nil {
		t.Fatalf("expected nil tenant, got %v", got)
	}
	want := uuid.New()
	got := cd.TenantIDFromContext(cd.WithTenantID(context.Background(), want))
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
