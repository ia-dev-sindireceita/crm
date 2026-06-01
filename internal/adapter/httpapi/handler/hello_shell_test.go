package handler_test

// SIN-63935 / UX-F1 — hello-tenant migrated to shell.Layout. The tests
// in this file pin the chrome contract that the migration introduces:
//
//   - the design-system stylesheets (tokens / components / app-shell)
//     are linked in the documented order, and the legacy auth.css link
//     survives via the new `head_extra` block,
//   - the top-bar brand renders the tenant name as both the aria-label
//     and the brand-text span,
//   - the top-bar nav renders ONLY the live surfaces with short labels
//     (disabled surfaces stay in the body list, never bleed into the
//     chrome),
//   - the user-menu carries the "Sair" logout option as a POST form
//     (with CSRF) AND the "Configurar 2FA" link,
//   - the hamburger toggle is always present in the DOM so keyboard
//     users can collapse the nav on mobile.
//
// The companion ordering / disabled-state assertions still live in
// hello_index_test.go; they do not duplicate the chrome pins here.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

func shellHelloRequest(t *testing.T) *http.Request {
	t.Helper()
	tenantID := uuid.New()
	userID := uuid.New()
	tenant := &tenancy.Tenant{ID: tenantID, Name: "acme", Host: "acme.crm.local"}
	r := tenantedRequest(t, http.MethodGet, "/hello-tenant", nil, tenant)
	return r.WithContext(middleware.WithSession(r.Context(), iam.Session{
		ID: uuid.New(), UserID: userID, TenantID: tenantID,
		CSRFToken: "shell-test-csrf",
		ExpiresAt: time.Now().Add(time.Hour),
	}))
}

func TestNewHelloTenant_ShellChromeLoadsStylesheets(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(handler.HelloTenantDeps{})(rec, shellHelloRequest(t))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		`<link rel="stylesheet" href="/static/css/tokens.css">`,
		`<link rel="stylesheet" href="/static/css/components.css">`,
		`<link rel="stylesheet" href="/static/css/app-shell.css">`,
		// SIN-63935 B-1 — app-shell.js wires hamburger + user-menu toggles.
		`<script src="/static/js/app-shell.js" defer></script>`,
		// SIN-63294 — auth.css must survive via the new head_extra slot.
		`<link rel="stylesheet" href="/static/css/auth.css" />`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// Ordering — tokens before components before app-shell. Cascade
	// breaks if components/app-shell load earlier than the tokens they
	// reference.
	tokensAt := strings.Index(body, "tokens.css")
	componentsAt := strings.Index(body, "components.css")
	appShellAt := strings.Index(body, "app-shell.css")
	if !(tokensAt < componentsAt && componentsAt < appShellAt) {
		t.Fatalf("css order wrong: tokens=%d components=%d app-shell=%d", tokensAt, componentsAt, appShellAt)
	}
}

func TestNewHelloTenant_ShellTopBarRendersTenantBrand(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(handler.HelloTenantDeps{})(rec, shellHelloRequest(t))
	body := rec.Body.String()

	if !strings.Contains(body, `class="app-shell__topbar"`) {
		t.Fatalf("body missing top-bar chrome: %q", body)
	}
	if !strings.Contains(body, `class="app-shell__brand"`) {
		t.Fatalf("body missing brand anchor: %q", body)
	}
	if !strings.Contains(body, `aria-label="acme"`) {
		t.Fatalf("body missing brand aria-label: %q", body)
	}
	if !strings.Contains(body, `<span class="app-shell__brand-text">acme</span>`) {
		t.Fatalf("body missing brand-text span: %q", body)
	}
}

func TestNewHelloTenant_ShellNavLiveSurfacesOnly(t *testing.T) {
	// Mix of enabled + disabled surfaces. Only TopNav=true AND
	// Available=true entries appear in the shell top-bar. Privacy and
	// 2FA are body-only surfaces (TopNav=false) and never appear in the
	// chrome regardless of their Available flag.
	t.Parallel()
	deps := handler.HelloTenantDeps{
		FunnelEnabled:    true,
		CatalogEnabled:   true,
		CampaignsEnabled: false, // disabled — must not be in top-bar
		PrivacyEnabled:   true,  // enabled but TopNav=false — body-only
		AIPolicyEnabled:  false, // disabled
	}
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(deps)(rec, shellHelloRequest(t))
	body := rec.Body.String()

	// TopNav=true, Available=true → must appear as nav anchors.
	for _, want := range []string{
		`<a href="/funnel">Funil</a>`,
		`<a href="/catalog">Catálogo</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing short-label nav link %q", want)
		}
	}

	// Disabled or body-only: must NOT appear as top-bar anchors.
	for _, off := range []string{
		`<a href="/campaigns">Campanhas</a>`,
		`<a href="/settings/ai-policy">IA</a>`,
		// Privacy is Available=true but TopNav=false — body-only per AC §2.
		`<a href="/settings/privacy">Privacidade</a>`,
	} {
		if strings.Contains(body, off) {
			t.Errorf("surface should not appear in top-bar nav: %q", off)
		}
	}
}

func TestNewHelloTenant_ShellUserMenuLogoutAndTwoFA(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(handler.HelloTenantDeps{})(rec, shellHelloRequest(t))
	body := rec.Body.String()

	// Toggle button is present and aria-correct.
	if !strings.Contains(body, `class="app-shell__user-menu-toggle"`) {
		t.Errorf("body missing user-menu toggle button: %q", body)
	}
	if !strings.Contains(body, `aria-haspopup="menu"`) {
		t.Errorf("body missing user-menu aria-haspopup: %q", body)
	}

	// 2FA item is a plain link.
	if !strings.Contains(body, `<a href="/admin/2fa/setup" role="menuitem">Configurar 2FA</a>`) {
		t.Errorf("body missing 2FA menu link: %q", body)
	}

	// Logout item is a POST form with CSRF hidden field.
	if !strings.Contains(body, `<form method="POST" action="/logout">`) {
		t.Errorf("body missing logout form: %q", body)
	}
	if !strings.Contains(body, `value="shell-test-csrf"`) {
		t.Errorf("body missing csrf token value in logout form: %q", body)
	}
	if !strings.Contains(body, `<button type="submit" role="menuitem">Sair</button>`) {
		t.Errorf("body missing logout submit button: %q", body)
	}
}

func TestNewHelloTenant_ShellHamburgerAlwaysInDOM(t *testing.T) {
	// CSS-driven mobile collapse is asserted in cmd/server
	// design_system_css_static_test.go (the @media rule must be
	// present). Here we pin that the button is in DOM regardless of
	// viewport, with the right aria attributes for keyboard users.
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(handler.HelloTenantDeps{})(rec, shellHelloRequest(t))
	body := rec.Body.String()

	for _, want := range []string{
		`class="app-shell__hamburger"`,
		`aria-controls="app-shell-nav"`,
		`aria-expanded="false"`,
		`aria-label="Abrir menu"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing hamburger attribute %q", want)
		}
	}
}
