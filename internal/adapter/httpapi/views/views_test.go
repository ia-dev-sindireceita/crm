package views_test

// Smoke tests for the views package. The handler tests already render
// the parsed templates end-to-end; these tests pin the package-local
// invariants (every page name resolves, the CSRF helpers wired into
// FuncMap survive a render with a non-empty token) so a refactor of
// views.go cannot silently strip the FuncMap.

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/views"
	"github.com/pericles-luz/crm/internal/branding"
)

// TestLogin_LayoutRenders covers the GET /login data shape. CSRFToken
// is empty because the user is not yet authenticated; the layout's
// {{if .CSRFToken}} guards must keep the meta tag and hx-headers
// attribute out of the response on this path (prevents writing an
// empty `content=""` meta that an XSS-bound JS would mistake for a
// real token).
func TestLogin_LayoutRenders(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	data := struct {
		Next      string
		Error     string
		CSRFToken string
	}{Next: "/hello-tenant"}
	if err := views.Login.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `<form method="POST" action="/login">`) {
		t.Fatalf("login form not rendered: %q", got)
	}
	if strings.Contains(got, `name="csrf-token"`) {
		t.Fatal("csrf <meta> tag must NOT render when CSRFToken is empty (login is pre-auth)")
	}
	if strings.Contains(got, "hx-headers") {
		t.Fatal("hx-headers attribute must NOT render when CSRFToken is empty")
	}
}

// TestHello_LayoutRendersWithCSRF asserts that, given a non-empty
// CSRFToken, the authenticated layout writes both the <meta> tag and
// the hx-headers attribute on <body>, plus the hidden form input on
// the logout form. Without these, the production CSRF middleware
// (mounted on the authed group) would reject every POST /logout from
// a real browser session.
func TestHello_LayoutRendersWithCSRF(t *testing.T) {
	t.Parallel()
	const token = "abc-csrf-token-xyz"
	var buf bytes.Buffer
	data := struct {
		TenantName string
		UserID     string
		CSRFToken  string
	}{TenantName: "acme", UserID: "user-1", CSRFToken: token}
	if err := views.Hello.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `<meta name="csrf-token" content="`+token+`">`) {
		t.Fatalf("csrf meta missing or wrong value: %q", got)
	}
	if !strings.Contains(got, `hx-headers='{"X-CSRF-Token": "`+token+`"}'`) {
		t.Fatalf("hx-headers attribute missing or wrong: %q", got)
	}
	if !strings.Contains(got, `<input type="hidden" name="_csrf" value="`+token+`">`) {
		t.Fatalf("hidden form input missing: %q", got)
	}
	if !strings.Contains(got, `<form method="POST" action="/logout">`) {
		t.Fatalf("logout form not rendered as POST: %q", got)
	}
}

// TestHello_LayoutRendersWithoutCSRF covers the legacy / migration
// fallback: a session row pre-dating migration 0011 carries an empty
// CSRFToken. The layout still renders successfully (no template
// runtime error) and silently omits the meta tag, hx-headers, and
// hidden input. The CSRF middleware will then reject the next write
// attempt with csrf.cookie_missing — fail-closed is the right answer.
func TestHello_LayoutRendersWithoutCSRF(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	data := struct {
		TenantName string
		UserID     string
		CSRFToken  string
	}{TenantName: "acme", UserID: "user-1"}
	if err := views.Hello.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, `name="csrf-token"`) {
		t.Fatal("csrf <meta> rendered when CSRFToken is empty")
	}
	if strings.Contains(got, "hx-headers") {
		t.Fatal("hx-headers rendered when CSRFToken is empty")
	}
}

// TestLayout_RendersTenantThemeStyle covers the SIN-63085 slot: when
// a non-empty TenantThemeStyle is supplied (the production path —
// middleware.Theme always attaches at least DefaultThemeStyle), the
// layout emits a <style id="tenant-theme">…</style> block inside
// <head>. The id is asserted exactly because the HTMX layer relies
// on it to know the style survives across swaps (head is never
// targeted by hx-swap).
func TestLayout_RendersTenantThemeStyle(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	var buf bytes.Buffer
	data := struct {
		Next             string
		Error            string
		CSRFToken        string
		TenantThemeStyle template.CSS
	}{Next: "/hello-tenant", TenantThemeStyle: style}
	if err := views.Login.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	// SIN-63275: the tenant-theme tag now always carries `nonce="…"`.
	// This test renders without setting CSPNonce, so the helper returns
	// an empty string and the layout emits `nonce=""` (intentional
	// fail-closed when middleware is absent). The omit-when-empty
	// regression is covered separately by TestLayout_OmitsTenantThemeStyleWhenEmpty.
	wantTag := `<style id="tenant-theme" nonce="">` + string(style) + `</style>`
	if !strings.Contains(got, wantTag) {
		t.Fatalf("layout did not render tenant theme tag.\nwant fragment: %q\nrendered: %q", wantTag, got)
	}
	if !strings.Contains(got, "--color-primary:#1f6feb") {
		t.Fatalf("CSS variables not present in <style>: %q", got)
	}
}

// TestLayout_OmitsTenantThemeStyleWhenEmpty pins the {{with}} guard:
// when the handler did not attach a theme style (a legacy code path,
// or a 500 fallback render) the layout MUST NOT emit an empty
// <style id="tenant-theme"></style> tag. Empty stylesheet tags are
// fine functionally but make the snapshot regression test noisier
// than it needs to be.
func TestLayout_OmitsTenantThemeStyleWhenEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	data := struct {
		Next             string
		Error            string
		CSRFToken        string
		TenantThemeStyle template.CSS
	}{Next: "/hello-tenant"}
	if err := views.Login.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	if strings.Contains(buf.String(), `id="tenant-theme"`) {
		t.Fatalf("empty TenantThemeStyle must not emit <style> tag: %q", buf.String())
	}
}

// TestLayout_StampsCSPNonceOnTenantTheme pins the SIN-63275 wireup:
// when both TenantThemeStyle and CSPNonce are populated the layout's
// <style id="tenant-theme"> carries the per-request nonce. Without
// this the strict `style-src 'self' 'nonce-…'` policy (no
// `'unsafe-inline'`) blocks the stylesheet in the browser.
func TestLayout_StampsCSPNonceOnTenantTheme(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	const nonce = "test-csp-nonce-xyz"
	var buf bytes.Buffer
	data := struct {
		Next             string
		Error            string
		CSRFToken        string
		TenantThemeStyle template.CSS
		CSPNonce         string
	}{Next: "/hello-tenant", TenantThemeStyle: style, CSPNonce: nonce}
	if err := views.Login.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	wantTag := `<style id="tenant-theme" nonce="` + nonce + `">` + string(style) + `</style>`
	if !strings.Contains(got, wantTag) {
		t.Fatalf("layout did not stamp CSP nonce on tenant-theme.\nwant fragment: %q\nrendered: %q", wantTag, got)
	}
}

// TestLayout_CSPNonceEmptyStillEmitsAttribute pins the fail-closed
// semantics. csp.Nonce returns "" when the middleware is missing; in
// that case the layout still emits nonce="" — an empty nonce never
// matches a CSP directive so the browser blocks the inline <style>
// (csp.Nonce docstring). Silently dropping the attribute would let an
// XSS-bound style sneak through if a future refactor relaxes the
// CSP policy.
func TestLayout_CSPNonceEmptyStillEmitsAttribute(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	var buf bytes.Buffer
	data := struct {
		Next             string
		Error            string
		CSRFToken        string
		TenantThemeStyle template.CSS
		CSPNonce         string
	}{Next: "/hello-tenant", TenantThemeStyle: style}
	if err := views.Login.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `<style id="tenant-theme" nonce="">`) {
		t.Fatalf("layout did not emit empty nonce attribute when CSPNonce is empty: %q", got)
	}
}

// TestLayout_CSPNonceEscapedAgainstInjection asserts the
// html/template escaping path: a forged nonce containing quotes or
// angle brackets must NOT break out of the attribute value. The
// helper returns a string (not template.HTMLAttr) so the html/template
// engine HTML-escapes it. A future refactor that swaps in
// template.HTMLAttr without sanitisation would let an attacker inject
// arbitrary attributes; this test fails fast in that case.
func TestLayout_CSPNonceEscapedAgainstInjection(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	const adversarial = `"><script>alert(1)</script>`
	var buf bytes.Buffer
	data := struct {
		Next             string
		Error            string
		CSRFToken        string
		TenantThemeStyle template.CSS
		CSPNonce         string
	}{Next: "/hello-tenant", TenantThemeStyle: style, CSPNonce: adversarial}
	if err := views.Login.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "<script>alert(1)</script>") {
		t.Fatalf("adversarial nonce escaped attribute scope: %q", got)
	}
}

// TestHello_LayoutEscapesAdversarialCSRF asserts the html/template
// escaping path on the CSRF helpers: a forged token containing quotes
// or angle brackets must NOT break out of the attribute value. The
// helpers (csrf.MetaTag / HXHeadersAttr / FormHidden) HTML-escape the
// token before interpolation; this test fails fast if a future
// refactor swaps in a raw string concat.
func TestHello_LayoutEscapesAdversarialCSRF(t *testing.T) {
	t.Parallel()
	const adversarial = `"><script>alert(1)</script>`
	var buf bytes.Buffer
	data := struct {
		TenantName string
		UserID     string
		CSRFToken  string
	}{TenantName: "acme", UserID: "user-1", CSRFToken: adversarial}
	if err := views.Hello.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "<script>alert(1)</script>") {
		t.Fatalf("adversarial token was not escaped: %q", got)
	}
}
