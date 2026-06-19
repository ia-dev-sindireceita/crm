package usermfa

// SIN-63963 / UX-F4 — the MFA-aware credential-failure re-render wires
// the tenant-settings BrandingReader so logo + white-label match the
// GET /login render. A nil reader or a read failure must degrade to the
// word-mark + platform footer (no 500, no blank card).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

type fakeBrandingReader struct {
	out tenancy.TenantBranding
	err error
}

func (f fakeBrandingReader) LoadBranding(_ context.Context, _ uuid.UUID) (tenancy.TenantBranding, error) {
	if f.err != nil {
		return tenancy.TenantBranding{}, f.err
	}
	return f.out, nil
}

func brandingLoginConfig(reader tenancy.BrandingReader) LoginConfig {
	return LoginConfig{
		IAM:          &fakeLoginIAM{err: iam.ErrInvalidCredentials},
		Sessions:     &fakeSessionDeleter{},
		Pendings:     &fakePendingCreator{},
		Requirements: &fakeRequirements{},
		PendingTTL:   5 * time.Minute,
		Branding:     reader,
	}
}

func postBadCredentials(t *testing.T, cfg LoginConfig, tenant *tenancy.Tenant) *httptest.ResponseRecorder {
	t.Helper()
	h := LoginPost(cfg)
	w := httptest.NewRecorder()
	form := url.Values{"email": []string{"alice@acme.test"}, "password": []string{"wrong"}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if tenant != nil {
		r = r.WithContext(tenancy.WithContext(context.Background(), tenant))
	}
	h(w, r)
	return w
}

func TestLoginPost_MFA_CredentialFailureRendersBrandingFromReader(t *testing.T) {
	t.Parallel()
	reader := fakeBrandingReader{out: tenancy.TenantBranding{
		LogoURL:    "https://static.example/t/acme/logo.svg",
		WhiteLabel: true,
	}}
	w := postBadCredentials(t, brandingLoginConfig(reader), &tenancy.Tenant{
		ID:   uuid.New(),
		Name: "Acme Corp",
	})

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `src="https://static.example/t/acme/logo.svg"`) {
		t.Fatalf("tenant logo missing on MFA credential-failure render: %q", body)
	}
	if strings.Contains(body, `data-testid="login-platform-footer"`) {
		t.Fatalf("footer must be suppressed under white-label: %q", body)
	}
}

func TestLoginPost_MFA_CredentialFailureFallsBackWhenReaderFails(t *testing.T) {
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
			reader := fakeBrandingReader{err: tc.err}
			w := postBadCredentials(t, brandingLoginConfig(reader), &tenancy.Tenant{
				ID:   uuid.New(),
				Name: "Acme Corp",
			})

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status: want 401 got %d", w.Code)
			}
			body := w.Body.String()
			if !strings.Contains(body, `data-testid="login-platform-footer"`) {
				t.Fatalf("footer must render on branding read failure: %q", body)
			}
			if strings.Contains(body, `data-testid="login-tenant-logo"`) {
				t.Fatalf("no logo expected on read failure: %q", body)
			}
		})
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
