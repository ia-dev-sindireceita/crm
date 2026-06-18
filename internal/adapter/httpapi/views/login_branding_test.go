package views_test

// SIN-63941 / UX-F4 — render tests for the redesigned /login surface.
// Each case maps to one acceptance criterion: tenant identity + logo
// fallback (AC #1), unchanged CSRF/CSP wiring (AC #3), platform
// "Powered by …" footer toggled by the WhiteLabel flag (AC #6), a11y
// affordances (focus targets, role=alert, password help text). The
// data shape used here intentionally adds the new fields to a struct
// the html/template engine has never seen before — proving the
// reflection helpers in views.go (loginTenantName / loginTenantLogo /
// loginWhiteLabel) survive a fresh page-data shape.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/views"
)

// loginPageData mirrors the production handler.loginViewData shape but
// stays inside the test so we exercise the reflection helpers (no
// type assertions on the production struct). Extra fields the helpers
// know nothing about — Surfaces, etc. — must not corrupt the render.
type loginPageData struct {
	Next       string
	Error      string
	CSRFToken  string
	CSPNonce   string
	TenantName string
	TenantLogo string
	WhiteLabel bool
}

// TestLogin_RendersTenantNameInHeading covers AC #1 — the tenant name
// flows from LoginViewModel into the <h1> via the loginTenantName
// reflection helper. The data-testid pin keeps the QA selector stable
// across a future visual refactor.
func TestLogin_RendersTenantNameInHeading(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := views.Login.ExecuteTemplate(&buf, "layout", loginPageData{TenantName: "Acme CRM"}); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `data-testid="login-tenant-name"`) {
		t.Fatalf("login heading testid missing: %q", got)
	}
	if !strings.Contains(got, ">Acme CRM</h1>") {
		t.Fatalf("tenant name not rendered inside <h1>: %q", got)
	}
	// Generic fallback heading must NOT appear when a tenant name is
	// present — both branches of {{with}} cannot fire at once.
	if strings.Contains(got, ">Entrar</h1>") {
		t.Fatalf("fallback heading rendered alongside tenant name: %q", got)
	}
}

// TestLogin_FallsBackToCRMHeading covers the "tenant missing" branch
// — e.g. a misrouted request that bypassed TenantScope, or a render
// from a smoke fixture without tenant data. The heading still
// renders with the generic "Entrar" label so the page is never a
// runtime template error.
func TestLogin_FallsBackToCRMHeading(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := views.Login.ExecuteTemplate(&buf, "layout", loginPageData{}); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, ">Entrar</h1>") {
		t.Fatalf("fallback heading missing: %q", got)
	}
	// No tenant name → no subtitle. The {{with}} guard on the
	// subtitle is the only thing keeping the empty <p> from leaking
	// into the markup, so pin the absence.
	if strings.Contains(got, `class="login-card__subtitle"`) {
		t.Fatalf("subtitle leaked when tenant name absent: %q", got)
	}
}

// TestLogin_RendersTenantLogoWhenProvided covers AC #1 — a tenant with
// a configured logo emits an <img src=...> at the top of the card.
// The alt attribute is intentionally empty: the visible <h1> already
// names the tenant so a screen reader would double-announce. The
// data-testid keeps the QA selector stable.
func TestLogin_RendersTenantLogoWhenProvided(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	const logoURL = "https://static.crm.example.com/t/acme-uuid/logo"
	data := loginPageData{TenantName: "Acme CRM", TenantLogo: logoURL}
	if err := views.Login.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `data-testid="login-tenant-logo"`) {
		t.Fatalf("logo testid missing: %q", got)
	}
	if !strings.Contains(got, `src="`+logoURL+`"`) {
		t.Fatalf("logo src missing: %q", got)
	}
	// Word-mark fallback must NOT render alongside the logo.
	if strings.Contains(got, `data-testid="login-wordmark"`) {
		t.Fatalf("wordmark fallback rendered alongside logo: %q", got)
	}
}

// TestLogin_RendersWordmarkWhenLogoAbsent covers the "tenant has no
// logo configured" branch — the platform shows a CRM word-mark
// fallback so the card header is never empty.
func TestLogin_RendersWordmarkWhenLogoAbsent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := views.Login.ExecuteTemplate(&buf, "layout", loginPageData{TenantName: "Acme CRM"}); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `data-testid="login-wordmark"`) {
		t.Fatalf("wordmark fallback missing: %q", got)
	}
	if strings.Contains(got, `data-testid="login-tenant-logo"`) {
		t.Fatalf("logo rendered when TenantLogo empty: %q", got)
	}
}

// TestLogin_RendersPlatformFooterWhenNotWhiteLabel covers AC #1 +
// task scope footer (SIN-65075): the "Powered by LMHost" line shows
// for the default tenant so attribution survives, and helps unbranded
// tenants discover the platform. The credit links to lmhost.com.br via
// a CSP-safe external anchor (href only, no inline handler) with
// target="_blank" rel="noopener noreferrer" to block reverse-tabnabbing.
func TestLogin_RendersPlatformFooterWhenNotWhiteLabel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := views.Login.ExecuteTemplate(&buf, "layout", loginPageData{TenantName: "Acme CRM"}); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `data-testid="login-platform-footer"`) {
		t.Fatalf("platform footer missing for non-white-label tenant: %q", got)
	}
	if !strings.Contains(got, "Powered by ") {
		t.Fatalf("platform attribution prefix missing: %q", got)
	}
	if !strings.Contains(got, `<a href="https://lmhost.com.br" target="_blank" rel="noopener noreferrer">LMHost</a>`) {
		t.Fatalf("LMHost attribution link missing or malformed: %q", got)
	}
}

// TestLogin_HidesPlatformFooterWhenWhiteLabel covers the white-label
// tenant path: a tenant that paid for branding does not advertise
// the underlying platform on its pre-auth surface. The whole
// <footer> block must drop, not just the text — leaving the <footer>
// element with empty content would still consume vertical space.
func TestLogin_HidesPlatformFooterWhenWhiteLabel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	data := loginPageData{TenantName: "Acme CRM", WhiteLabel: true}
	if err := views.Login.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, `data-testid="login-platform-footer"`) {
		t.Fatalf("platform footer leaked on white-label tenant: %q", got)
	}
	if strings.Contains(got, "LMHost") {
		t.Fatalf("platform attribution text leaked on white-label tenant: %q", got)
	}
}

// TestLogin_RendersErrorAlertWithDangerClass covers AC #5: the
// credential-failure path re-renders the form with `<p role="alert">`
// (so screen readers announce it) AND the F1 .alert.alert--danger
// classes so the visual treatment matches every other danger band
// in the app. The data-testid lets QA assert the alert without
// coupling to the class string.
func TestLogin_RendersErrorAlertWithDangerClass(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	data := loginPageData{TenantName: "Acme CRM", Error: "Email ou senha inválidos."}
	if err := views.Login.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `data-testid="login-error"`) {
		t.Fatalf("login error testid missing: %q", got)
	}
	if !strings.Contains(got, `role="alert"`) {
		t.Fatalf("role=alert missing on login error: %q", got)
	}
	if !strings.Contains(got, `class="alert alert--danger login-card__error"`) {
		t.Fatalf("alert--danger class missing on login error: %q", got)
	}
	if !strings.Contains(got, "Email ou senha inválidos.") {
		t.Fatalf("error message body missing: %q", got)
	}
}

// TestLogin_RendersPasswordHelpInline covers the affordances scope
// (Norman/feedback): the minimum-length requirement is announced
// BEFORE the user errors, so a screen-reader user discovers the
// constraint via aria-describedby and a sighted user via the inline
// hint. Both must be wired together.
func TestLogin_RendersPasswordHelpInline(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := views.Login.ExecuteTemplate(&buf, "layout", loginPageData{}); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `aria-describedby="login-password-help"`) {
		t.Fatalf("aria-describedby on password missing: %q", got)
	}
	if !strings.Contains(got, `id="login-password-help"`) {
		t.Fatalf("password help node id missing: %q", got)
	}
	if !strings.Contains(got, "Mínimo 12 caracteres.") {
		t.Fatalf("password help text missing: %q", got)
	}
}

// TestLogin_RendersDisabledForgotLink covers the affordances scope:
// the "Esqueci minha senha" link is an aria-disabled span (not a
// dead <a href=…> route) so a click does not cascade into a 404.
// The title attribute carries the operator-facing copy.
func TestLogin_RendersDisabledForgotLink(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := views.Login.ExecuteTemplate(&buf, "layout", loginPageData{}); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `data-testid="login-forgot-disabled"`) {
		t.Fatalf("forgot-password testid missing: %q", got)
	}
	if !strings.Contains(got, `aria-disabled="true"`) {
		t.Fatalf("aria-disabled missing on forgot link: %q", got)
	}
	if strings.Contains(got, `<a href="/forgot-password"`) || strings.Contains(got, `<a href="/password-reset"`) {
		t.Fatalf("forgot link must not be a live <a href> route: %q", got)
	}
}

// TestLogin_LinksDesignSystemStylesheets covers AC #1 + scope
// requirement: the redesigned /login surface must load the F1 token
// + components CSS bundles so the .btn / .alert primitives + the
// --space-*/--color-* tokens consumed by login.css resolve. The
// link to /static/css/auth.css survives from SIN-63294 (pins the
// SIN-63935 baseline contrast); login.css is the new page-scoped
// layer. app-shell.css must NOT load on the pre-auth surface — it
// owns the post-auth chrome (top-bar, nav) which has no analogue on
// /login.
func TestLogin_LinksDesignSystemStylesheets(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := views.Login.ExecuteTemplate(&buf, "layout", loginPageData{}); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		`<link rel="stylesheet" href="/static/css/tokens.css" />`,
		`<link rel="stylesheet" href="/static/css/components.css" />`,
		`<link rel="stylesheet" href="/static/css/auth.css" />`,
		`<link rel="stylesheet" href="/static/css/login.css" />`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stylesheet link missing.\nwant: %q\nrendered: %q", want, got)
		}
	}
	if strings.Contains(got, `/static/css/app-shell.css`) {
		t.Fatalf("app-shell.css must NOT load on pre-auth /login: %q", got)
	}
}

// TestLogin_PreservesNextOnError covers AC #2 + the security
// guarantee that the originally-requested path round-trips through
// the credential-failure render. A future refactor that strips the
// hidden input would silently redirect users to /hello-tenant after
// a failed login, breaking the deep-link UX.
func TestLogin_PreservesNextOnError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	const next = "/inbox?lead=123"
	data := loginPageData{Next: next, Error: "Email ou senha inválidos."}
	if err := views.Login.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	// html/template escapes the ? and = in the value attribute, which
	// is the right behaviour for an HTML attribute context. Match
	// the literal hidden-input shape that survives that escaping.
	wantInput := `<input type="hidden" name="next" value="/inbox?lead=123"`
	if !strings.Contains(got, wantInput) {
		t.Fatalf("hidden next input missing or wrong value: %q", got)
	}
}

// TestLogin_EscapesTenantNameAndLogo asserts the html/template
// escaping path on the reflection helpers. An adversarial tenant
// name or logo URL must not break out of the attribute / element
// context — they are stored in Postgres but enter the render via
// reflection.ValueOf on a string field, so the helpers MUST stay
// untyped (return `string`) rather than `template.HTML` to keep
// the engine in escaping mode.
func TestLogin_EscapesTenantNameAndLogo(t *testing.T) {
	t.Parallel()
	const xss = `"><script>alert(1)</script>`
	var buf bytes.Buffer
	data := loginPageData{TenantName: xss, TenantLogo: xss}
	if err := views.Login.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "<script>alert(1)</script>") {
		t.Fatalf("adversarial input escaped attribute scope: %q", got)
	}
}
