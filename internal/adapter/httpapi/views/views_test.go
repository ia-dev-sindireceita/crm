package views_test

// Smoke tests for the views package. The handler tests already render
// the parsed templates end-to-end; these tests pin the package-local
// invariants (every page name resolves, the CSRF helpers wired into
// FuncMap survive a render with a non-empty token) so a refactor of
// views.go cannot silently strip the FuncMap.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/views"
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
