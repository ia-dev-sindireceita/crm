package shell_test

import (
	"bytes"
	"embed"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/web/shell"
)

//go:embed testdata/*.html
var testAssets embed.FS

// renderShell parses the shell layout with the canonical testdata
// content page and renders the supplied data. Returns the rendered
// HTML and any execute error.
func renderShell(t *testing.T, data any) string {
	t.Helper()
	tmpl, err := shell.Parse(nil, testAssets, "testdata/content.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := shell.Render(&buf, tmpl, data); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// pageData is the canonical embedding shape consumers use. Each
// feature handler builds one of these (or its own struct with the
// same field names) before calling shell.Render.
type pageData struct {
	shell.Data
}

func TestRender_BrandShowsTenantNameAndLogo(t *testing.T) {
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{
		TenantName: "Acme Co",
		TenantLogo: "/branding/logo.png",
	}})

	mustContain(t, body, `class="app-shell__brand"`)
	mustContain(t, body, `aria-label="Acme Co"`)
	mustContain(t, body, `<img src="/branding/logo.png" alt="Acme Co">`)
	mustContain(t, body, `<span class="app-shell__brand-text">Acme Co</span>`)
}

func TestRender_BrandFallsBackToPeithoWhenTenantNameEmpty(t *testing.T) {
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{}})

	mustContain(t, body, `<span class="app-shell__brand-text">Peitho</span>`)
	mustNotContain(t, body, "<img src=")
}

func TestRender_NavItemsFilteredByRole_Atendente(t *testing.T) {
	// Atendente sees Inbox + Funil only (no manager surfaces). Caller
	// is responsible for the filter; this test pins that the shell
	// renders exactly what it gets.
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{
		NavItems: []shell.NavItem{
			{Label: "Inbox", Path: "/inbox", Active: true},
			{Label: "Funil", Path: "/funnel"},
		},
	}})

	mustContain(t, body, `<a href="/inbox" aria-current="page">Inbox</a>`)
	mustContain(t, body, `<a href="/funnel">Funil</a>`)
	mustNotContain(t, body, "/catalog")
	mustNotContain(t, body, "/campaigns")
}

func TestRender_NavItemsFilteredByRole_Gerente(t *testing.T) {
	// Gerente has full visibility — every surface mounted.
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{
		NavItems: []shell.NavItem{
			{Label: "Inbox", Path: "/inbox"},
			{Label: "Funil", Path: "/funnel"},
			{Label: "Catálogo", Path: "/catalog"},
			{Label: "Campanhas", Path: "/campaigns"},
			{Label: "Branding", Path: "/branding", Active: true},
		},
	}})

	mustContain(t, body, `<a href="/inbox">Inbox</a>`)
	mustContain(t, body, `<a href="/funnel">Funil</a>`)
	mustContain(t, body, `<a href="/catalog">Catálogo</a>`)
	mustContain(t, body, `<a href="/campaigns">Campanhas</a>`)
	mustContain(t, body, `<a href="/branding" aria-current="page">Branding</a>`)
}

func TestRender_NavItemsFilteredByRole_Lider(t *testing.T) {
	// Líder = atendente surfaces + funnel rules (team lead can edit
	// stage transitions). Still no manager-only surfaces (catalog,
	// campaigns, branding).
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{
		NavItems: []shell.NavItem{
			{Label: "Inbox", Path: "/inbox"},
			{Label: "Funil", Path: "/funnel"},
			{Label: "Regras", Path: "/funnel/rules", Active: true},
		},
	}})

	mustContain(t, body, `<a href="/inbox">Inbox</a>`)
	mustContain(t, body, `<a href="/funnel">Funil</a>`)
	mustContain(t, body, `<a href="/funnel/rules" aria-current="page">Regras</a>`)
	mustNotContain(t, body, "/catalog")
	mustNotContain(t, body, "/campaigns")
	mustNotContain(t, body, "/branding")
}

func TestRender_NavItemsFilteredByRole_Master(t *testing.T) {
	// Master = full visibility PLUS the cross-tenant master surface.
	// Pin that an extra entry beyond the gerente set renders without
	// the shell rejecting unknown labels.
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{
		NavItems: []shell.NavItem{
			{Label: "Inbox", Path: "/inbox"},
			{Label: "Funil", Path: "/funnel"},
			{Label: "Catálogo", Path: "/catalog"},
			{Label: "Campanhas", Path: "/campaigns"},
			{Label: "Branding", Path: "/branding"},
			{Label: "Tenants", Path: "/master/tenants", Active: true},
		},
	}})

	mustContain(t, body, `<a href="/inbox">Inbox</a>`)
	mustContain(t, body, `<a href="/funnel">Funil</a>`)
	mustContain(t, body, `<a href="/catalog">Catálogo</a>`)
	mustContain(t, body, `<a href="/campaigns">Campanhas</a>`)
	mustContain(t, body, `<a href="/branding">Branding</a>`)
	mustContain(t, body, `<a href="/master/tenants" aria-current="page">Tenants</a>`)
}

func TestRender_NavItemsEmpty_NavBlockOmitted(t *testing.T) {
	// Tests the empty-slice branch: with no NavItems the <nav> block
	// is skipped entirely so screen readers don't announce an empty
	// landmark.
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{}})

	mustNotContain(t, body, `id="app-shell-nav"`)
}

func TestRender_AriaCurrentMarksActiveRoute(t *testing.T) {
	// One Active=true item produces exactly one aria-current="page"
	// attribute. Inactive items must not carry it.
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{
		NavItems: []shell.NavItem{
			{Label: "Inbox", Path: "/inbox"},
			{Label: "Funil", Path: "/funnel", Active: true},
			{Label: "Catálogo", Path: "/catalog"},
		},
	}})

	if got := strings.Count(body, `aria-current="page"`); got != 1 {
		t.Fatalf("aria-current count = %d, want 1; body=%s", got, body)
	}
	mustContain(t, body, `href="/funnel" aria-current="page"`)
}

func TestRender_UserMenuLogoutIsPOSTWithCSRF(t *testing.T) {
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{
		UserDisplayName: "Maria Atendente",
		CSRFToken:       "tkn-XYZ-123",
		UserMenuItems: []shell.UserMenuItem{
			{Label: "2FA", Path: "/admin/2fa/setup"},
			{Label: "Sair", Path: "/logout", Form: true},
		},
	}})

	mustContain(t, body, `<button type="button"`)
	mustContain(t, body, `aria-haspopup="menu"`)
	mustContain(t, body, "Maria Atendente")
	mustContain(t, body, `<a href="/admin/2fa/setup" role="menuitem">2FA</a>`)
	mustContain(t, body, `<form method="POST" action="/logout">`)
	mustContain(t, body, `value="tkn-XYZ-123"`)
	mustContain(t, body, `<button type="submit" role="menuitem">Sair</button>`)
}

func TestRender_UserMenuFallsBackToContaWhenDisplayNameEmpty(t *testing.T) {
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{}})
	mustContain(t, body, "Conta")
}

func TestRender_CSPNonceStampedOnTenantThemeStyle(t *testing.T) {
	// SIN-63275 — every inline <style> must carry the per-request
	// nonce or the browser blocks it under the no-unsafe-inline CSP.
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{
		CSPNonce:         "n0nc3-2026",
		TenantThemeStyle: template.CSS(":root{--color-primary:#abcdef}"),
	}})

	mustContain(t, body, `<style id="tenant-theme" nonce="n0nc3-2026">:root{--color-primary:#abcdef}</style>`)
}

func TestRender_NoTenantThemeStyle_StyleBlockOmitted(t *testing.T) {
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{CSPNonce: "n"}})
	mustNotContain(t, body, `id="tenant-theme"`)
}

func TestRender_CSRFTokenWiresMetaAndBodyHeaders(t *testing.T) {
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{CSRFToken: "csrf-abc"}})

	mustContain(t, body, `<meta name="csrf-token" content="csrf-abc">`)
	mustContain(t, body, `hx-headers=`)
}

func TestRender_CSRFTokenEmpty_NoMetaNoHXHeaders(t *testing.T) {
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{}})

	mustNotContain(t, body, `name="csrf-token"`)
	mustNotContain(t, body, `hx-headers`)
}

func TestRender_StaticCSSAssetsLoadedInOrder(t *testing.T) {
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{}})

	tokensAt := strings.Index(body, "tokens.css")
	componentsAt := strings.Index(body, "components.css")
	appShellAt := strings.Index(body, "app-shell.css")

	if tokensAt < 0 || componentsAt < 0 || appShellAt < 0 {
		t.Fatalf("missing one of the css links: tokens=%d components=%d app-shell=%d", tokensAt, componentsAt, appShellAt)
	}
	if !(tokensAt < componentsAt && componentsAt < appShellAt) {
		t.Fatalf("css order wrong: tokens=%d components=%d app-shell=%d", tokensAt, componentsAt, appShellAt)
	}
}

func TestRender_AppShellToggleScriptIsLinked(t *testing.T) {
	// SIN-63935 B-1 regression guard. The hamburger and user-menu
	// toggles only function when /static/js/app-shell.js is loaded; a
	// future cascade refactor that drops the <script> tag here would
	// silently break AC #4 (keyboard-only toggle works) at runtime
	// without breaking a single existing test. Pin the link so any
	// such regression fails CI.
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{}})

	mustContain(t, body, `<script src="/static/js/app-shell.js" defer></script>`)
}

func TestRender_HamburgerHasMinHitTargetAttributes(t *testing.T) {
	// Mobile collapse is CSS-driven (@media max-width: 599px) but the
	// hamburger button must always be present in DOM with the right
	// aria attributes so keyboard navigation works in every breakpoint.
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{
		NavItems: []shell.NavItem{{Label: "Inbox", Path: "/inbox"}},
	}})

	mustContain(t, body, `class="app-shell__hamburger"`)
	mustContain(t, body, `aria-controls="app-shell-nav"`)
	mustContain(t, body, `aria-expanded="false"`)
	mustContain(t, body, `aria-label="Abrir menu"`)
}

func TestRender_ContentBlockIsHonoured(t *testing.T) {
	// Sanity that the content block from testdata/content.html lands
	// inside <main class="app-shell__main">.
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{TenantName: "X"}})

	mustContain(t, body, `<main class="app-shell__main"`)
	mustContain(t, body, `data-testid="content-marker"`)
}

func TestRender_TitleBlockOverridesDefault(t *testing.T) {
	t.Parallel()
	body := renderShell(t, pageData{shell.Data{}})

	// content.html defines {{define "title"}} so we get its value,
	// not the default "Peitho".
	mustContain(t, body, "<title>shell-test-title</title>")
}

func TestMustParse_PanicsOnBadFile(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	shell.MustParse(nil, testAssets, "testdata/does-not-exist.html")
}

func TestMustParse_NoContentFilesReturnsLayoutOnly(t *testing.T) {
	t.Parallel()
	tmpl := shell.MustParse(nil, testAssets)
	if tmpl == nil {
		t.Fatal("nil template")
	}
	if tmpl.Lookup("layout") == nil {
		t.Fatal("layout block missing")
	}
}

func TestParse_ExtraFuncsOverrideBase(t *testing.T) {
	// Caller-supplied funcs in extraFuncs must win over BaseFuncs on
	// name collision. Override shellTenantName to a marker and confirm
	// the marker, not the data field, lands in the output.
	t.Parallel()
	extras := template.FuncMap{
		"shellTenantName": func(any) string { return "OVERRIDDEN" },
	}
	tmpl, err := shell.Parse(extras, testAssets, "testdata/content.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := shell.Render(&buf, tmpl, pageData{shell.Data{TenantName: "Acme"}}); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	mustContain(t, body, "OVERRIDDEN")
	mustNotContain(t, body, ">Acme<")
}

// ----- reflection-helper coverage --------------------------------------

func TestData_NonStructTypes_FallBackToDefaults(t *testing.T) {
	t.Parallel()
	// Renders against a non-struct (string) data — every reflection
	// helper hits the "not a struct" branch and the layout still
	// produces a valid document with defaults.
	tmpl, err := shell.Parse(nil, testAssets, "testdata/content.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := shell.Render(&buf, tmpl, "not a struct"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	// fallback tenant name and default user-menu label
	mustContain(t, body, ">Peitho<")
	mustContain(t, body, "Conta")
}

func TestData_NilData_FallBackToDefaults(t *testing.T) {
	t.Parallel()
	tmpl, err := shell.Parse(nil, testAssets, "testdata/content.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	// nil interface — every helper unwrap path bails to defaults.
	if err := shell.Render(&buf, tmpl, nil); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	mustContain(t, body, ">Peitho<")
}

func TestData_StringFieldForTenantThemeStyle(t *testing.T) {
	// shellTenantThemeStyle accepts both template.CSS and raw string.
	// Pin both for parity with the views package helper.
	t.Parallel()
	type legacyData struct {
		shell.Data
		// Override the embedded TenantThemeStyle field via shadowing:
		// because legacyData declares TenantThemeStyle directly the
		// outer field wins.
		TenantThemeStyle string
		CSPNonce         string
	}
	body := renderShell(t, legacyData{
		TenantThemeStyle: ":root{--color-primary:#f00}",
		CSPNonce:         "n",
	})
	mustContain(t, body, `:root{--color-primary:#f00}`)
}

func TestData_WrongFieldTypesIgnored(t *testing.T) {
	// A struct with the right names but wrong types — every helper
	// should refuse and fall back to defaults instead of panicking
	// inside reflect.
	t.Parallel()
	type wrongTypes struct {
		TenantName       int
		NavItems         string
		UserMenuItems    int
		TenantThemeStyle int
		CSRFToken        int
		CSPNonce         int
		UserDisplayName  int
	}
	tmpl, err := shell.Parse(nil, testAssets, "testdata/content.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := shell.Render(&buf, tmpl, wrongTypes{TenantName: 42, NavItems: "no"}); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	mustContain(t, body, ">Peitho<")
	mustContain(t, body, "Conta")
}

func TestData_NilPointerFallsBack(t *testing.T) {
	t.Parallel()
	tmpl, err := shell.Parse(nil, testAssets, "testdata/content.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := shell.Render(&buf, tmpl, (*pageData)(nil)); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	mustContain(t, body, ">Peitho<")
}

func TestNavItems_NonSliceFieldType(t *testing.T) {
	// Field present but holding the wrong concrete type — the helper
	// must not panic.
	t.Parallel()
	type weirdNav struct {
		NavItems string
	}
	tmpl, err := shell.Parse(nil, testAssets, "testdata/content.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := shell.Render(&buf, tmpl, weirdNav{NavItems: "not a slice"}); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	mustNotContain(t, body, `id="app-shell-nav"`)
}

// ----- helpers ---------------------------------------------------------

func mustContain(t *testing.T, body, needle string) {
	t.Helper()
	if !strings.Contains(body, needle) {
		t.Fatalf("body missing %q\n---body---\n%s", needle, body)
	}
}

func mustNotContain(t *testing.T, body, needle string) {
	t.Helper()
	if strings.Contains(body, needle) {
		t.Fatalf("body unexpectedly contains %q\n---body---\n%s", needle, body)
	}
}
