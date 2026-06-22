package inbox

import (
	"bytes"
	"strings"
	"testing"
)

// TestInboxLayout_RendersAppShellChrome pins SIN-65104: the full-page
// inbox layout is composed on the global SidebarNav app-shell
// (internal/web/shell). The chrome (sidebar, primary nav, brand, user
// menu) must render around the inbox content so the inbox matches the
// Pitho mock instead of the old standalone full-viewport <main>.
func TestInboxLayout_RendersAppShellChrome(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := inboxLayoutTmpl.Execute(&buf, layoutData{
		TenantName:      "Pitho",
		UserDisplayName: "atendente",
		NavItems:        buildInboxNavItems(),
		UserMenuItems:   buildInboxUserMenu(),
		CSRFToken:       "csrf-token-shell",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		// app-shell chrome owned by shell.Layout
		`class="app-shell"`,
		`class="app-shell__sidebar"`,
		`id="app-shell-nav"`,
		`class="app-shell__main"`,
		// primary nav: Inbox active + Funil
		`href="/inbox"`,
		`aria-current="page"`,
		`href="/funnel"`,
		// user menu logout (form-based, CSRF hidden carried by the shell)
		`action="/logout"`,
		`Sair`,
		// the inbox surface still renders inside the content slot
		`data-testid="inbox-shell"`,
		`data-testid="inbox-list-pane"`,
		`data-testid="inbox-conversation-pane"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inbox layout missing %q after shell migration", want)
		}
	}
}

// TestInboxLayout_NoNestedMain pins that the inbox surface no longer
// declares its own <main>/role="main": the shell layout owns the single
// role="main" landmark (a11y — one main per document). The inbox-shell is
// now a <div> inside the shell's main column.
func TestInboxLayout_NoNestedMain(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := inboxLayoutTmpl.Execute(&buf, layoutData{
		NavItems:      buildInboxNavItems(),
		UserMenuItems: buildInboxUserMenu(),
		CSRFToken:     "csrf",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	if n := strings.Count(out, `role="main"`); n != 1 {
		t.Errorf(`want exactly one role="main" landmark (shell-owned); got %d`, n)
	}
	if strings.Contains(out, `<main class="inbox-shell"`) {
		t.Errorf("inbox-shell must be a <div> inside the shell main, not a nested <main>")
	}
}

// TestBuildInboxNavItems pins the inbox primary nav: Inbox active, Funil
// inactive — mirroring funnel's nav so the two post-login surfaces share
// one SidebarNav (SIN-65104).
func TestBuildInboxNavItems(t *testing.T) {
	t.Parallel()
	items := buildInboxNavItems()
	if len(items) != 2 {
		t.Fatalf("want 2 nav items, got %d", len(items))
	}
	if items[0].Label != "Inbox" || items[0].Path != "/inbox" || !items[0].Active {
		t.Errorf("first nav item must be active Inbox -> /inbox, got %+v", items[0])
	}
	if items[1].Label != "Funil" || items[1].Path != "/funnel" || items[1].Active {
		t.Errorf("second nav item must be inactive Funil -> /funnel, got %+v", items[1])
	}
}

// TestBuildInboxUserMenu pins the inbox user-menu: a single form-based
// logout entry (SIN-65104).
func TestBuildInboxUserMenu(t *testing.T) {
	t.Parallel()
	items := buildInboxUserMenu()
	if len(items) != 1 {
		t.Fatalf("want 1 user-menu item, got %d", len(items))
	}
	if items[0].Label != "Sair" || items[0].Path != "/logout" || !items[0].Form {
		t.Errorf("user-menu item must be form-based Sair -> /logout, got %+v", items[0])
	}
}
