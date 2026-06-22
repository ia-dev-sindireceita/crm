package dashboard

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestPage_RendersAppShellChrome pins SIN-65122: the full-navigation
// dashboard page is composed on the global SidebarNav app-shell
// (internal/web/shell). The chrome (sidebar, primary nav, brand, user
// menu) renders around the dashboard grid so the screen matches the
// Pitho rollout instead of the old standalone full-page document.
func TestPage_RendersAppShellChrome(t *testing.T) {
	t.Parallel()
	fake := &fakeSnapshot{snap: sampleSnapshot()}
	rec := serve(t, mustHandler(t, Deps{Snapshot: fake}), "GET", "/dashboard", uuid.New(), true)
	if rec.Code != 200 {
		t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		// app-shell chrome owned by shell.Layout
		`class="app-shell"`,
		`class="app-shell__sidebar"`,
		`id="app-shell-nav"`,
		`class="app-shell__main"`,
		// design-system stylesheet linked by the shell head + the page sheet
		// injected via head_extra.
		`/static/css/app-shell.css`,
		`/static/css/dashboard.css`,
		// primary nav: the seed-role set, with Painel active.
		`href="/inbox"`,
		`href="/funnel"`,
		`href="/contacts"`,
		`<a href="/dashboard" aria-current="page">Painel</a>`,
		// user-menu logout (form-based, lives in the shell)
		`action="/logout"`,
		`Sair`,
		// the dashboard surface still renders inside the content slot
		`data-testid="dashboard"`,
		`data-testid="dashboard-counters"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard page missing %q after shell migration", want)
		}
	}
}

// TestPage_SingleMainLandmark pins that the dashboard no longer declares
// its own <main>/role="main": the shell layout owns the single role="main"
// landmark (a11y — one main per document). The dashboard grid is now a
// <div> inside the shell main column.
func TestPage_SingleMainLandmark(t *testing.T) {
	t.Parallel()
	fake := &fakeSnapshot{snap: sampleSnapshot()}
	rec := serve(t, mustHandler(t, Deps{Snapshot: fake}), "GET", "/dashboard", uuid.New(), true)
	body := rec.Body.String()
	if n := strings.Count(body, `role="main"`); n != 1 {
		t.Errorf(`want exactly one role="main" landmark (shell-owned); got %d`, n)
	}
	if strings.Contains(body, `<main class="dashboard"`) {
		t.Errorf("dashboard must be a <div> inside the shell main, not a nested <main>")
	}
}

// TestBuildDashboardNavItems pins the dashboard primary nav: the seed-role
// surface set with Painel active (SIN-65122).
func TestBuildDashboardNavItems(t *testing.T) {
	t.Parallel()
	items := buildDashboardNavItems()
	want := []struct {
		label, path string
		active      bool
	}{
		{"Inbox", "/inbox", false},
		{"Funil", "/funnel", false},
		{"Contatos", "/contacts", false},
		{"Painel", "/dashboard", true},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d nav items, want %d", len(items), len(want))
	}
	for i, w := range want {
		if items[i].Label != w.label || items[i].Path != w.path || items[i].Active != w.active {
			t.Errorf("nav[%d] = %+v, want {%s %s active=%v}", i, items[i], w.label, w.path, w.active)
		}
	}
}

// TestBuildDashboardUserMenu pins the single form-based logout entry.
func TestBuildDashboardUserMenu(t *testing.T) {
	t.Parallel()
	items := buildDashboardUserMenu()
	if len(items) != 1 {
		t.Fatalf("want 1 user-menu item, got %d", len(items))
	}
	if items[0].Label != "Sair" || items[0].Path != "/logout" || !items[0].Form {
		t.Errorf("user-menu item must be form-based Sair -> /logout, got %+v", items[0])
	}
}

// TestDisplayNameForUser pins the placeholder display formatter.
func TestDisplayNameForUser(t *testing.T) {
	t.Parallel()
	if got := displayNameForUser(uuid.Nil); got != "Conta" {
		t.Errorf("nil user -> %q, want Conta", got)
	}
	id := uuid.New()
	if got := displayNameForUser(id); got != id.String()[:8] {
		t.Errorf("non-nil user -> %q, want %q", got, id.String()[:8])
	}
}
