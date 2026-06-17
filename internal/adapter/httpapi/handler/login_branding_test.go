package handler_test

// SIN-63941 / UX-F4 — handler-side coverage for the redesigned /login
// surface: the legacy LoginGet + the credential-failure re-render
// both stamp the tenant name from tenancy.FromContext, both load the
// F1 stylesheet bundle (regression guard against a future refactor
// that drops the link tags from layout.html), and the "Powered by
// LMHost" footer (SIN-65075) renders on the default tenant because
// the handler does not yet emit WhiteLabel=true (tenant-settings
// read port lands in a follow-up issue).

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

func TestLoginGet_RendersTenantNameFromContext(t *testing.T) {
	t.Parallel()
	r := tenantedRequest(t, http.MethodGet, "/login", nil, &tenancy.Tenant{
		ID:   uuid.New(),
		Name: "Acme Corp",
		Host: "acme.crm.local",
	})
	rec := httptest.NewRecorder()

	handler.LoginGet(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ">Acme Corp</h1>") {
		t.Fatalf("tenant name not rendered inside <h1>: %q", body)
	}
	if !strings.Contains(body, `data-testid="login-tenant-name"`) {
		t.Fatalf("login-tenant-name testid missing: %q", body)
	}
}

// TestLoginGet_FallsBackWhenTenantMissing covers the misroute branch:
// a request that reaches LoginGet without a tenant in context (TenantScope
// bypassed, e.g. a direct unit-test render) still produces a renderable
// page rather than a 500 template error. The fallback heading is the
// generic "Entrar" word the page template owns.
func TestLoginGet_FallsBackWhenTenantMissing(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()

	handler.LoginGet(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ">Entrar</h1>") {
		t.Fatalf("fallback heading missing when tenant missing: %q", body)
	}
}

// TestLoginGet_LinksFullStylesheetBundle pins the SIN-63941 layout
// wiring: every link tag the redesign relies on (tokens, components,
// auth, login) must round-trip through the handler. A future refactor
// that splits the layout into per-feature head blocks could silently
// drop one of them and reproduce the SIN-63294 "tela sem formatação"
// bug on the pre-auth surface.
func TestLoginGet_LinksFullStylesheetBundle(t *testing.T) {
	t.Parallel()
	r := tenantedRequest(t, http.MethodGet, "/login", nil, &tenancy.Tenant{
		ID:   uuid.New(),
		Name: "Acme Corp",
	})
	rec := httptest.NewRecorder()

	handler.LoginGet(rec, r)

	body := rec.Body.String()
	for _, want := range []string{
		`/static/css/tokens.css`,
		`/static/css/components.css`,
		`/static/css/auth.css`,
		`/static/css/login.css`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stylesheet link missing: %q\nbody: %q", want, body)
		}
	}
}

// TestLoginGet_PlatformFooterOnDefaultTenant pins the white-label
// default: the handler does not yet emit WhiteLabel=true so every
// tenant currently sees the platform attribution footer. When the
// tenant-settings read port (follow-up) wires WhiteLabel into the
// handler, this test will need to grow a "WhiteLabel=true → no footer"
// twin — adding that twin is explicit, not a silent regression.
func TestLoginGet_PlatformFooterOnDefaultTenant(t *testing.T) {
	t.Parallel()
	r := tenantedRequest(t, http.MethodGet, "/login", nil, &tenancy.Tenant{
		ID:   uuid.New(),
		Name: "Acme Corp",
	})
	rec := httptest.NewRecorder()

	handler.LoginGet(rec, r)

	body := rec.Body.String()
	if !strings.Contains(body, `Powered by <a href="https://lmhost.com.br" target="_blank" rel="noopener noreferrer">LMHost</a>`) {
		t.Fatalf("LMHost platform footer missing on default tenant: %q", body)
	}
}

// TestLoginPost_CredentialFailureRendersBrandedCard covers AC #2 (no
// regression) — the credential-failure re-render still resolves the
// tenant from context and still emits the alert via role="alert" +
// the F1 .alert--danger class. iam.ErrInvalidCredentials is the
// branch that lands the user back on /login with the form
// re-rendered; any other error goes through the 429/500 translator
// in loginhandler.WriteLoginError and is out of scope here.
func TestLoginPost_CredentialFailureRendersBrandedCard(t *testing.T) {
	t.Parallel()
	iamFake := &fakeIAM{loginErr: iam.ErrInvalidCredentials}
	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "wrong")
	r := tenantedRequest(t, http.MethodPost, "/login",
		strings.NewReader(form.Encode()),
		&tenancy.Tenant{ID: uuid.New(), Name: "Acme Corp"},
	)
	rec := httptest.NewRecorder()

	handler.LoginPost(handler.LoginConfig{IAM: iamFake})(rec, r)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ">Acme Corp</h1>") {
		t.Fatalf("tenant name missing on credential-failure render: %q", body)
	}
	if !strings.Contains(body, `class="alert alert--danger login-card__error"`) {
		t.Fatalf("alert--danger class missing on credential-failure render: %q", body)
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Fatalf("role=alert missing on credential-failure render: %q", body)
	}
}
