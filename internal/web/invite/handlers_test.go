package invite_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/password"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/tenantusers"
	webinvite "github.com/pericles-luz/crm/internal/web/invite"
)

// fakeService is a hand-rolled tenantusers.CredentialService stand-in.
type fakeService struct {
	resolve     func(ctx context.Context, tenantID uuid.UUID, tok string) (tenantusers.Invite, error)
	setPassword func(ctx context.Context, tenantID uuid.UUID, tok, pw string, pctx password.PolicyContext) (tenantusers.Invite, error)
}

func (f *fakeService) Resolve(ctx context.Context, tenantID uuid.UUID, tok string) (tenantusers.Invite, error) {
	return f.resolve(ctx, tenantID, tok)
}

func (f *fakeService) SetPassword(ctx context.Context, tenantID uuid.UUID, tok, pw string, pctx password.PolicyContext) (tenantusers.Invite, error) {
	return f.setPassword(ctx, tenantID, tok, pw, pctx)
}

func newHandler(t *testing.T, svc webinvite.Service) http.Handler {
	t.Helper()
	h, err := webinvite.New(webinvite.Deps{Service: svc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func withTenant(r *http.Request) *http.Request {
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "Acme", Host: "acme.crm.local"}
	return r.WithContext(tenancy.WithContext(r.Context(), tenant))
}

func TestNew_NilServiceRejected(t *testing.T) {
	if _, err := webinvite.New(webinvite.Deps{}); err == nil {
		t.Fatal("expected error for nil Service")
	}
}

func TestShow_ValidToken_RendersForm(t *testing.T) {
	svc := &fakeService{
		resolve: func(_ context.Context, _ uuid.UUID, tok string) (tenantusers.Invite, error) {
			if tok != "good" {
				t.Fatalf("token=%q", tok)
			}
			return tenantusers.Invite{UserID: uuid.New(), Email: "u@ex.com"}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/invite/good", nil))
	newHandler(t, svc).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `action="/invite/good"`) {
		t.Fatalf("form action missing: %s", body)
	}
	if !strings.Contains(body, "u@ex.com") {
		t.Fatalf("email not shown")
	}
	if !strings.Contains(body, `name="password"`) || !strings.Contains(body, `name="password_confirm"`) {
		t.Fatalf("password fields missing")
	}
	if got := rr.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("referrer-policy=%q", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("cache-control=%q", got)
	}
}

func TestShow_InvalidToken_GenericError(t *testing.T) {
	svc := &fakeService{
		resolve: func(_ context.Context, _ uuid.UUID, _ string) (tenantusers.Invite, error) {
			return tenantusers.Invite{}, tenantusers.ErrTokenInvalid
		},
	}
	rr := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/invite/whatever", nil))
	newHandler(t, svc).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rr.Code)
	}
	body := rr.Body.String()
	// No oracle: one generic message covers all three failure causes.
	if !strings.Contains(body, "inválido, expirou ou já foi utilizado") {
		t.Fatalf("generic error copy missing: %s", body)
	}
	// The form must not render on the error page.
	if strings.Contains(body, `name="password"`) {
		t.Fatalf("password form leaked on error page")
	}
}

func TestShow_NoTenant_500(t *testing.T) {
	svc := &fakeService{resolve: func(context.Context, uuid.UUID, string) (tenantusers.Invite, error) {
		t.Fatal("resolve should not be called without a tenant")
		return tenantusers.Invite{}, nil
	}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/invite/x", nil) // no tenant in ctx
	newHandler(t, svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", rr.Code)
	}
}

func postForm(t *testing.T, h http.Handler, tok, pw, confirm string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"password": {pw}, "password_confirm": {confirm}}
	req := httptest.NewRequest(http.MethodPost, "/invite/"+tok, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = withTenant(req)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestSubmit_Success(t *testing.T) {
	var gotPctx password.PolicyContext
	svc := &fakeService{
		setPassword: func(_ context.Context, _ uuid.UUID, tok, pw string, pctx password.PolicyContext) (tenantusers.Invite, error) {
			gotPctx = pctx
			if tok != "good" || pw != "Str0ng-Passphrase!" {
				t.Fatalf("tok=%q pw=%q", tok, pw)
			}
			return tenantusers.Invite{UserID: uuid.New(), Email: "u@ex.com"}, nil
		},
	}
	rr := postForm(t, newHandler(t, svc), "good", "Str0ng-Passphrase!", "Str0ng-Passphrase!")
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Senha definida com sucesso") {
		t.Fatalf("success page missing: %s", rr.Body.String())
	}
	if gotPctx.TenantName != "Acme" {
		t.Fatalf("tenant name not passed to policy: %+v", gotPctx)
	}
}

func TestSubmit_Mismatch_ReResolvesForm(t *testing.T) {
	resolveCalls := 0
	svc := &fakeService{
		resolve: func(_ context.Context, _ uuid.UUID, _ string) (tenantusers.Invite, error) {
			resolveCalls++
			return tenantusers.Invite{Email: "u@ex.com"}, nil
		},
		setPassword: func(context.Context, uuid.UUID, string, string, password.PolicyContext) (tenantusers.Invite, error) {
			t.Fatal("SetPassword must not be called when passwords mismatch")
			return tenantusers.Invite{}, nil
		},
	}
	rr := postForm(t, newHandler(t, svc), "good", "aaaaaaaaaaaa", "bbbbbbbbbbbb")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d, want 422", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "não coincidem") {
		t.Fatalf("mismatch message missing")
	}
	if resolveCalls != 1 {
		t.Fatalf("resolve calls=%d, want 1 (re-render header)", resolveCalls)
	}
}

func TestSubmit_PolicyError_ReRendersLocalized(t *testing.T) {
	cases := map[password.PolicyReason]string{
		password.ReasonTooShort:        "muito curta",
		password.ReasonTooLong:         "muito longa",
		password.ReasonMatchesIdentity: "igual ao seu e-mail",
		password.ReasonBreached:        "vazamentos conhecidos",
		password.PolicyReason("weird"): "política",
	}
	for reason, want := range cases {
		t.Run(string(reason), func(t *testing.T) {
			svc := &fakeService{
				resolve: func(context.Context, uuid.UUID, string) (tenantusers.Invite, error) {
					return tenantusers.Invite{Email: "u@ex.com"}, nil
				},
				setPassword: func(context.Context, uuid.UUID, string, string, password.PolicyContext) (tenantusers.Invite, error) {
					return tenantusers.Invite{}, &password.PolicyError{Reason: reason, Detail: "x"}
				},
			}
			rr := postForm(t, newHandler(t, svc), "good", "weakpassword", "weakpassword")
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("code=%d, want 422", rr.Code)
			}
			if !strings.Contains(rr.Body.String(), want) {
				t.Fatalf("reason %q: body missing %q", reason, want)
			}
			// Form re-renders so the invitee can retry.
			if !strings.Contains(rr.Body.String(), `name="password"`) {
				t.Fatalf("form not re-rendered for reason %q", reason)
			}
		})
	}
}

func TestSubmit_TokenInvalid_GenericError(t *testing.T) {
	svc := &fakeService{
		setPassword: func(context.Context, uuid.UUID, string, string, password.PolicyContext) (tenantusers.Invite, error) {
			return tenantusers.Invite{}, tenantusers.ErrTokenInvalid
		},
	}
	rr := postForm(t, newHandler(t, svc), "spent", "Str0ng-Passphrase!", "Str0ng-Passphrase!")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "inválido, expirou ou já foi utilizado") {
		t.Fatalf("generic error missing")
	}
}

func TestSubmit_InfraError_500(t *testing.T) {
	svc := &fakeService{
		setPassword: func(context.Context, uuid.UUID, string, string, password.PolicyContext) (tenantusers.Invite, error) {
			return tenantusers.Invite{}, errors.New("db down")
		},
	}
	rr := postForm(t, newHandler(t, svc), "good", "Str0ng-Passphrase!", "Str0ng-Passphrase!")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", rr.Code)
	}
}

func TestSubmit_PolicyError_TokenSinceInvalidated(t *testing.T) {
	// Policy fails AND the re-resolve for the form header finds the token now
	// invalid → fall through to the generic error page (not a broken form).
	svc := &fakeService{
		resolve: func(context.Context, uuid.UUID, string) (tenantusers.Invite, error) {
			return tenantusers.Invite{}, tenantusers.ErrTokenInvalid
		},
		setPassword: func(context.Context, uuid.UUID, string, string, password.PolicyContext) (tenantusers.Invite, error) {
			return tenantusers.Invite{}, &password.PolicyError{Reason: password.ReasonBreached}
		},
	}
	rr := postForm(t, newHandler(t, svc), "good", "weakpassword", "weakpassword")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rr.Code)
	}
}
