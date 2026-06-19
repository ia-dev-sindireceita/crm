package handler_test

// SIN-63963 / UX-F4 — coverage for the tenant-settings branding read
// port wired into the /login surface. The GET render (LoginGetHandler)
// and the credential-failure re-render (LoginPost with a branding
// reader) both must fill TenantLogo + WhiteLabel from storage; a nil
// reader or a read failure must degrade to the word-mark + platform
// footer rather than 500.

import (
	"context"
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

// fakeBranding is a tenancy.BrandingReader test double. It records the
// tenant id it was queried with and returns a canned result/error.
type fakeBranding struct {
	out   tenancy.TenantBranding
	err   error
	gotID uuid.UUID
	calls int
}

func (f *fakeBranding) LoadBranding(_ context.Context, tenantID uuid.UUID) (tenancy.TenantBranding, error) {
	f.calls++
	f.gotID = tenantID
	if f.err != nil {
		return tenancy.TenantBranding{}, f.err
	}
	return f.out, nil
}

func TestLoginGetHandler_RendersTenantLogoFromReader(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	reader := &fakeBranding{out: tenancy.TenantBranding{
		LogoURL:    "https://static.example/t/acme/logo.svg",
		WhiteLabel: true,
	}}
	r := tenantedRequest(t, http.MethodGet, "/login", nil, &tenancy.Tenant{
		ID:   tenantID,
		Name: "Acme Corp",
	})
	rec := httptest.NewRecorder()

	handler.LoginGetHandler(handler.LoginConfig{Branding: reader})(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if reader.gotID != tenantID {
		t.Fatalf("reader queried tenant %s, want %s", reader.gotID, tenantID)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `src="https://static.example/t/acme/logo.svg"`) {
		t.Fatalf("tenant logo not rendered: %q", body)
	}
	if !strings.Contains(body, `data-testid="login-tenant-logo"`) {
		t.Fatalf("login-tenant-logo testid missing: %q", body)
	}
	// AC #2 twin: white-label tenant suppresses the platform footer.
	if strings.Contains(body, `data-testid="login-platform-footer"`) {
		t.Fatalf("platform footer must be suppressed when WhiteLabel=true: %q", body)
	}
}

// TestLoginGetHandler_WhiteLabelKeepsWordmarkWhenNoLogo covers a tenant
// that toggled white-label but never uploaded a logo: the footer is
// suppressed yet the word-mark fallback still renders so the card is
// never blank.
func TestLoginGetHandler_WhiteLabelKeepsWordmarkWhenNoLogo(t *testing.T) {
	t.Parallel()
	reader := &fakeBranding{out: tenancy.TenantBranding{WhiteLabel: true}}
	r := tenantedRequest(t, http.MethodGet, "/login", nil, &tenancy.Tenant{
		ID:   uuid.New(),
		Name: "Acme Corp",
	})
	rec := httptest.NewRecorder()

	handler.LoginGetHandler(handler.LoginConfig{Branding: reader})(rec, r)

	body := rec.Body.String()
	if strings.Contains(body, `data-testid="login-tenant-logo"`) {
		t.Fatalf("no logo configured but <img> rendered: %q", body)
	}
	if !strings.Contains(body, `data-testid="login-wordmark"`) {
		t.Fatalf("word-mark fallback missing: %q", body)
	}
	if strings.Contains(body, `data-testid="login-platform-footer"`) {
		t.Fatalf("footer must be suppressed under white-label: %q", body)
	}
}

// TestLoginGetHandler_FallsBackWhenReaderFails proves the surface never
// 500s on a storage fault: a failing reader degrades to the platform
// word-mark + footer (the nil-reader default).
func TestLoginGetHandler_FallsBackWhenReaderFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"transient error", errStub("boom")},
		{"tenant not found", tenancy.ErrTenantNotFound},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeBranding{err: tc.err}
			r := tenantedRequest(t, http.MethodGet, "/login", nil, &tenancy.Tenant{
				ID:   uuid.New(),
				Name: "Acme Corp",
			})
			rec := httptest.NewRecorder()

			handler.LoginGetHandler(handler.LoginConfig{Branding: reader})(rec, r)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `data-testid="login-platform-footer"`) {
				t.Fatalf("footer must render on branding read failure: %q", body)
			}
			if strings.Contains(body, `data-testid="login-tenant-logo"`) {
				t.Fatalf("no logo expected on read failure: %q", body)
			}
		})
	}
}

// TestLoginGetHandler_NilReaderMatchesLegacy pins that the wired
// constructor with a nil reader is byte-behaviour-equivalent to the
// bare LoginGet: footer present, no logo.
func TestLoginGetHandler_NilReaderMatchesLegacy(t *testing.T) {
	t.Parallel()
	r := tenantedRequest(t, http.MethodGet, "/login", nil, &tenancy.Tenant{
		ID:   uuid.New(),
		Name: "Acme Corp",
	})
	rec := httptest.NewRecorder()

	handler.LoginGetHandler(handler.LoginConfig{})(rec, r)

	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="login-platform-footer"`) {
		t.Fatalf("footer expected with nil reader: %q", body)
	}
	if strings.Contains(body, `data-testid="login-tenant-logo"`) {
		t.Fatalf("no logo expected with nil reader: %q", body)
	}
}

// TestLoginPost_CredentialFailureRendersBrandingFromReader covers the
// 401 re-render path: a bad password on a white-label tenant still
// brands the card with the configured logo and suppresses the footer.
func TestLoginPost_CredentialFailureRendersBrandingFromReader(t *testing.T) {
	t.Parallel()
	reader := &fakeBranding{out: tenancy.TenantBranding{
		LogoURL:    "https://static.example/t/acme/logo.svg",
		WhiteLabel: true,
	}}
	iamFake := &fakeIAM{loginErr: iam.ErrInvalidCredentials}
	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "wrong")
	r := tenantedRequest(t, http.MethodPost, "/login",
		strings.NewReader(form.Encode()),
		&tenancy.Tenant{ID: uuid.New(), Name: "Acme Corp"},
	)
	rec := httptest.NewRecorder()

	handler.LoginPost(handler.LoginConfig{IAM: iamFake, Branding: reader})(rec, r)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `src="https://static.example/t/acme/logo.svg"`) {
		t.Fatalf("tenant logo missing on credential-failure render: %q", body)
	}
	if strings.Contains(body, `data-testid="login-platform-footer"`) {
		t.Fatalf("footer must be suppressed on white-label credential failure: %q", body)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
