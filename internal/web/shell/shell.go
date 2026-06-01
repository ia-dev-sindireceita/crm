package shell

import (
	"embed"
	"html/template"
	"io"
	"io/fs"
	"reflect"

	csrfhelpers "github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
)

//go:embed layout.html
var assets embed.FS

// NavItem is one entry in the top-bar primary nav. Active marks the
// route the current request resolved to so the layout can stamp
// aria-current="page". Filtering by role happens in the caller — the
// shell receives the already-filtered slice and renders it verbatim.
type NavItem struct {
	Label  string
	Path   string
	Active bool
}

// UserMenuItem is one entry in the user-menu dropdown. Form=true emits
// a <form method="POST"> wrapper with the CSRF hidden field (used for
// logout); Form=false emits a plain <a href>. Path is mandatory.
type UserMenuItem struct {
	Label string
	Path  string
	Form  bool
}

// Data is the field set the shell layout reads off page data. Embed it
// into your feature's page data struct so the chrome stays decoupled
// from feature-specific fields. Equivalent name-and-type matches on a
// non-embedded struct work too — the FuncMap helpers read each field
// via reflection (see shell_funcs.go for the lookup branches).
type Data struct {
	// TenantName is the brand label rendered next to the optional
	// tenant logo. Empty defaults to "CRM".
	TenantName string

	// TenantLogo is the absolute URL of the tenant logo. Empty hides
	// the <img> element and renders just the text brand.
	TenantLogo string

	// UserDisplayName is the operator's label inside the user-menu
	// toggle. Empty defaults to "Conta".
	UserDisplayName string

	// NavItems is the primary nav list. Caller filters by role/permission.
	NavItems []NavItem

	// UserMenuItems is the dropdown list. Caller filters by role/permission.
	UserMenuItems []UserMenuItem

	// CSRFToken feeds the <meta>, hx-headers, and hidden-input slots.
	// Empty disables CSRF wiring (anonymous chrome).
	CSRFToken string

	// CSPNonce is stamped on every inline <style> the layout owns; the
	// SIN-63275 CSP middleware emits style-src 'self' 'nonce-…' without
	// 'unsafe-inline', so an empty value here fail-closes the tenant
	// theme to user-agent defaults.
	CSPNonce string

	// TenantThemeStyle is the pre-rendered :root{…} declaration from
	// branding.ThemeStyleFromContext. Empty falls back to the neutral
	// tokens in tokens.css.
	TenantThemeStyle template.CSS
}

// BaseFuncs returns the FuncMap the shell layout relies on. Callers
// composing additional templates pass their own funcs into Parse — the
// shell helpers are merged first, so caller funcs override on conflict.
func BaseFuncs() template.FuncMap {
	return template.FuncMap{
		"csrfMeta":              csrfhelpers.MetaTag,
		"csrfHXHeaders":         csrfhelpers.HXHeadersAttr,
		"csrfFormHidden":        csrfhelpers.FormHidden,
		"shellTenantName":       shellTenantName,
		"shellTenantLogo":       shellTenantLogo,
		"shellUserDisplayName":  shellUserDisplayName,
		"shellNavItems":         shellNavItems,
		"shellUserMenuItems":    shellUserMenuItems,
		"shellCSRFToken":        shellCSRFToken,
		"shellCSPNonce":         shellCSPNonce,
		"shellTenantThemeStyle": shellTenantThemeStyle,
	}
}

// Parse returns a fresh template tree composed of the shell layout
// plus caller-supplied content files. Each content file must define a
// "content" template (required) and may override "title" (optional).
// extraFuncs are merged on top of BaseFuncs — supply additional helpers
// or override CSRF/branding helpers for tests.
func Parse(extraFuncs template.FuncMap, contentFS fs.FS, contentFiles ...string) (*template.Template, error) {
	funcs := BaseFuncs()
	for k, v := range extraFuncs {
		funcs[k] = v
	}
	t, err := template.New("shell.layout").Funcs(funcs).ParseFS(assets, "layout.html")
	if err != nil {
		return nil, err
	}
	if len(contentFiles) == 0 {
		return t, nil
	}
	return t.ParseFS(contentFS, contentFiles...)
}

// MustParse panics on error — useful at package init where any failure
// is a programmer error (typo in the embedded source). Production
// handlers parse once at startup.
func MustParse(extraFuncs template.FuncMap, contentFS fs.FS, contentFiles ...string) *template.Template {
	t, err := Parse(extraFuncs, contentFS, contentFiles...)
	if err != nil {
		panic("internal/web/shell: " + err.Error())
	}
	return t
}

// Render executes the shell layout against w. Page data must carry the
// fields shell.Data declares (or embed shell.Data). Errors propagate
// to the caller; callers that have already written response headers
// should swallow them or 500.
func Render(w io.Writer, tmpl *template.Template, data any) error {
	return tmpl.ExecuteTemplate(w, "layout", data)
}

// -------- reflection-based field accessors -------------------------------
//
// The pattern mirrors internal/adapter/httpapi/views: page data structs
// are owned by per-feature packages and a single-issue PR cannot
// reasonably mutate every one. Reflection keeps the shell layout
// renderable against legacy structs that don't yet embed shell.Data.

func unwrap(data any) (reflect.Value, bool) {
	if data == nil {
		return reflect.Value{}, false
	}
	v := reflect.ValueOf(data)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	return v, true
}

func stringField(data any, name, fallback string) string {
	v, ok := unwrap(data)
	if !ok {
		return fallback
	}
	f := v.FieldByName(name)
	if !f.IsValid() {
		return fallback
	}
	if s, ok := f.Interface().(string); ok {
		if s == "" {
			return fallback
		}
		return s
	}
	return fallback
}

func shellTenantName(data any) string      { return stringField(data, "TenantName", "CRM") }
func shellTenantLogo(data any) string      { return stringField(data, "TenantLogo", "") }
func shellUserDisplayName(data any) string { return stringField(data, "UserDisplayName", "Conta") }
func shellCSRFToken(data any) string       { return stringField(data, "CSRFToken", "") }
func shellCSPNonce(data any) string        { return stringField(data, "CSPNonce", "") }

func shellTenantThemeStyle(data any) template.CSS {
	v, ok := unwrap(data)
	if !ok {
		return ""
	}
	f := v.FieldByName("TenantThemeStyle")
	if !f.IsValid() {
		return ""
	}
	switch x := f.Interface().(type) {
	case template.CSS:
		return x
	case string:
		return template.CSS(x)
	default:
		return ""
	}
}

func shellNavItems(data any) []NavItem {
	v, ok := unwrap(data)
	if !ok {
		return nil
	}
	f := v.FieldByName("NavItems")
	if !f.IsValid() {
		return nil
	}
	if s, ok := f.Interface().([]NavItem); ok {
		return s
	}
	return nil
}

func shellUserMenuItems(data any) []UserMenuItem {
	v, ok := unwrap(data)
	if !ok {
		return nil
	}
	f := v.FieldByName("UserMenuItems")
	if !f.IsValid() {
		return nil
	}
	if s, ok := f.Interface().([]UserMenuItem); ok {
		return s
	}
	return nil
}
