package contacts

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestContactLayout_RendersAppShellChrome pins SIN-65122: the full-page
// contact-identity view is composed on the global SidebarNav app-shell
// (internal/web/shell). The chrome (sidebar, primary nav, brand, user
// menu) renders around the contact-detail surface so the screen matches
// the Pitho rollout instead of the old standalone full-page document.
func TestContactLayout_RendersAppShellChrome(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := contactLayoutTmpl.Execute(&buf, layoutData{
		TenantName:      "Pitho",
		UserDisplayName: "atendente",
		NavItems:        buildContactsNavItems(),
		UserMenuItems:   buildContactsUserMenu(),
		CSRFToken:       "csrf-token-shell",
		Panel:           stubPanelData(),
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
		// design-system stylesheet linked by the shell head + page sheet via head_extra
		`/static/css/app-shell.css`,
		`/static/css/contacts.css`,
		// CSRF meta wired through the shell (was the page's own {{.CSRFMeta}})
		`<meta name="csrf-token" content="csrf-token-shell">`,
		// primary nav: the seed-role set, with Contatos active
		`href="/inbox"`,
		`href="/funnel"`,
		`<a href="/contacts" aria-current="page">Contatos</a>`,
		`href="/dashboard"`,
		// user-menu logout (form-based, lives in the shell, carries CSRF)
		`action="/logout"`,
		`Sair`,
		// the contact surface still renders inside the content slot
		`data-testid="contact-shell"`,
		`id="identity-panel"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("contact layout missing %q after shell migration", want)
		}
	}
}

// TestContactLayout_SingleMainLandmark pins that the contact view no
// longer declares its own <main>/role="main": the shell layout owns the
// single role="main" landmark (a11y — one main per document).
func TestContactLayout_SingleMainLandmark(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := contactLayoutTmpl.Execute(&buf, layoutData{
		NavItems:      buildContactsNavItems(),
		UserMenuItems: buildContactsUserMenu(),
		Panel:         stubPanelData(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	if n := strings.Count(out, `role="main"`); n != 1 {
		t.Errorf(`want exactly one role="main" landmark (shell-owned); got %d`, n)
	}
	if strings.Contains(out, `<main class="contact-shell"`) {
		t.Errorf("contact-shell must be a <div> inside the shell main, not a nested <main>")
	}
}

// TestContactsListAndEdit_RenderAppShell pins that the list and edit
// full-page surfaces also wrap in the shell, each highlighting Contatos.
func TestContactsListAndEdit_RenderAppShell(t *testing.T) {
	t.Parallel()
	var listBuf bytes.Buffer
	if err := contactsListTmpl.Execute(&listBuf, listLayoutData{
		TenantName:    "Pitho",
		NavItems:      buildContactsNavItems(),
		UserMenuItems: buildContactsUserMenu(),
		CSRFToken:     "csrf",
		Results:       resultsData{},
	}); err != nil {
		t.Fatalf("list Execute: %v", err)
	}
	list := listBuf.String()
	for _, want := range []string{`class="app-shell"`, `id="contacts-results"`, `<a href="/contacts" aria-current="page">Contatos</a>`, `data-testid="contacts-shell"`} {
		if !strings.Contains(list, want) {
			t.Errorf("contacts list missing %q after shell migration", want)
		}
	}

	var editBuf bytes.Buffer
	if err := contactEditPageTmpl.Execute(&editBuf, editLayoutData{
		TenantName:    "Pitho",
		NavItems:      buildContactsNavItems(),
		UserMenuItems: buildContactsUserMenu(),
		CSRFToken:     "csrf",
		Form:          editFormData{ContactID: uuid.New(), DisplayName: "Frank"},
	}); err != nil {
		t.Fatalf("edit Execute: %v", err)
	}
	edit := editBuf.String()
	for _, want := range []string{`class="app-shell"`, `id="contact-edit-panel"`, `<a href="/contacts" aria-current="page">Contatos</a>`} {
		if !strings.Contains(edit, want) {
			t.Errorf("contacts edit page missing %q after shell migration", want)
		}
	}
}

// TestBuildContactsNavItems pins the contacts primary nav: the seed-role
// surface set with Contatos active (SIN-65122).
func TestBuildContactsNavItems(t *testing.T) {
	t.Parallel()
	items := buildContactsNavItems()
	want := []struct {
		label, path string
		active      bool
	}{
		{"Inbox", "/inbox", false},
		{"Funil", "/funnel", false},
		{"Contatos", "/contacts", true},
		{"Painel", "/dashboard", false},
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

// TestBuildContactsUserMenu pins the single form-based logout entry.
func TestBuildContactsUserMenu(t *testing.T) {
	t.Parallel()
	items := buildContactsUserMenu()
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
