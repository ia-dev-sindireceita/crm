package handler_test

// SIN-65125 — /hello-tenant must link the theme-aware content sheet so
// the card copy + "Abrir" links flip with data-theme="dark". The sheet
// loads AFTER auth.css (whose body/main rules bind the non-flipping
// legacy tokens) so it wins the cascade; pin both the presence and the
// ordering here. The companion served-as-CSS guard lives in cmd/server
// hello_tenant_css_static_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
)

func TestNewHelloTenant_LinksHelloTenantThemeSheetAfterAuth(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(handler.HelloTenantDeps{})(rec, shellHelloRequest(t))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()

	const link = `<link rel="stylesheet" href="/static/css/hello-tenant.css" />`
	if !strings.Contains(body, link) {
		t.Fatalf("body missing hello-tenant.css link: %q", body)
	}

	// Must load after auth.css to override its legacy body/main binds.
	authAt := strings.Index(body, "/static/css/auth.css")
	helloAt := strings.Index(body, "/static/css/hello-tenant.css")
	if authAt < 0 || helloAt < 0 || !(authAt < helloAt) {
		t.Fatalf("hello-tenant.css must load after auth.css: auth=%d hello=%d", authAt, helloAt)
	}
}
