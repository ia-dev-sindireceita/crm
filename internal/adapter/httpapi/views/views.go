// Package views holds the html/template assets for the httpapi handlers.
//
// Each parsed *template.Template is composed of layout.html plus exactly
// one page template. ExecuteTemplate renders the "layout" block; the page
// templates supply "title" and "content" blocks. layout.html itself never
// loads remote assets — Content-Security-Policy stays at default-src
// 'self'.
//
// SIN-62217 ships only the bare templates needed to prove the auth →
// tenancy → RLS stack end-to-end. HTMX integration lives in a follow-up
// PR alongside the static-asset route; until then layout is plain HTML.
package views

import (
	"embed"
	"html/template"
	"reflect"

	csrfhelpers "github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
)

// tenantThemeStyle is the FuncMap helper that reads .TenantThemeStyle
// from the page data via reflection. The indirection lets the layout
// emit <style id="tenant-theme"> without forcing every handler's data
// struct to grow a new field — page templates and their data shapes
// are owned by per-feature packages (campaigns, inbox, master) and a
// SIN-63085-scoped PR cannot reasonably mutate every one.
//
// Returns:
//   - the field value when it exists and is a template.CSS or string,
//   - empty template.CSS otherwise (the {{with}} guard in the layout
//     then skips the <style> element entirely).
//
// Pure: no request state, no globals, safe across goroutines.
func tenantThemeStyle(data any) template.CSS {
	if data == nil {
		return ""
	}
	v := reflect.ValueOf(data)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
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

//go:embed *.html
var assets embed.FS

// csrfFuncs exposes the CSRF templ helpers as html/template FuncMap
// entries (ADR 0073 §D1). The functions are pure — no request state —
// so wiring them at parse time is safe across goroutines. Each helper
// returns a html/template-safe type (template.HTML / template.HTMLAttr)
// so the {{}} interpolation does not double-escape the already-escaped
// payload.
var csrfFuncs = template.FuncMap{
	"csrfMeta":         csrfhelpers.MetaTag,
	"csrfHXHeaders":    csrfhelpers.HXHeadersAttr,
	"csrfFormHidden":   csrfhelpers.FormHidden,
	"tenantThemeStyle": tenantThemeStyle,
}

// Login renders GET /login and the re-rendered POST /login form on
// credential failure. Data shape: struct { Next, Error string }.
var Login = template.Must(
	template.New("login").Funcs(csrfFuncs).ParseFS(assets, "layout.html", "login.html"),
)

// Hello renders GET /hello-tenant. Data shape: struct { TenantName,
// UserID, CSRFToken string }. CSRFToken is the per-session CSPRNG
// token from iam.Session.CSRFToken; it feeds the layout meta tag and
// the logout form hidden input. Empty-string is allowed (legacy
// session pre-dating migration 0011) — the helpers still render
// safely, and the CSRF middleware will reject the next write attempt.
var Hello = template.Must(
	template.New("hello").Funcs(csrfFuncs).ParseFS(assets, "layout.html", "hello.html"),
)
