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
	"github.com/pericles-luz/crm/internal/web/shell"
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

// Surface is one entry of the post-login navigable index rendered by
// hello.html (SIN-63774). Available controls the rendered shape:
// true → <a href="{{.Path}}">{{.Label}}</a>, false → an aria-disabled
// <span> so the gap is visible to the operator instead of dead-linking.
type Surface struct {
	Label     string
	Path      string
	Available bool
}

// helloSurfaces is the FuncMap helper that reads .Surfaces from page
// data via reflection. The reflection path mirrors tenantThemeStyle so
// hello.html stays renderable from views_test.go fixtures whose data
// structs predate SIN-63774 and do not carry a Surfaces field.
//
// Returns:
//   - the slice when the field exists and is a []Surface,
//   - nil otherwise — the {{with}} guard in hello.html then skips the
//     <nav> block entirely.
//
// Pure: no request state, no globals, safe across goroutines.
func helloSurfaces(data any) []Surface {
	if data == nil {
		return nil
	}
	v := reflect.ValueOf(data)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	f := v.FieldByName("Surfaces")
	if !f.IsValid() {
		return nil
	}
	if s, ok := f.Interface().([]Surface); ok {
		return s
	}
	return nil
}

// cspNonce is the FuncMap helper that reads .CSPNonce from the page
// data via reflection. SIN-63275 wired the CSP middleware to ship
// `style-src 'self' 'nonce-…'` without `'unsafe-inline'`; every
// <style> tag the layout owns therefore needs the per-request nonce
// or the browser blocks the stylesheet. Reflection keeps the layout
// agnostic to which per-feature data struct it is rendering — handlers
// that read csp.Nonce(r.Context()) into a `CSPNonce string` field will
// have it stamped automatically.
//
// Returns:
//   - the nonce string when the field exists and is a string,
//   - empty string otherwise — the layout still emits the attribute,
//     and an empty nonce never matches a CSP directive (fail-closed:
//     the inline <style> is blocked rather than silently allowed).
//
// Pure: no request state, no globals, safe across goroutines.
func cspNonce(data any) string {
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
	f := v.FieldByName("CSPNonce")
	if !f.IsValid() {
		return ""
	}
	if s, ok := f.Interface().(string); ok {
		return s
	}
	return ""
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
	"cspNonce":         cspNonce,
	"helloSurfaces":    helloSurfaces,
}

// Login renders GET /login and the re-rendered POST /login form on
// credential failure. Data shape: struct { Next, Error string }.
var Login = template.Must(
	template.New("login").Funcs(csrfFuncs).ParseFS(assets, "layout.html", "login.html"),
)

// Hello renders GET /hello-tenant. SIN-63935 / UX-F1 migrated this
// page to the shell.Layout app-shell so the post-login chrome (top-bar,
// branded nav, user-menu) is shared with every other authenticated
// surface. The page content (welcome string, surfaces nav, logout
// form) still lives in hello.html via the layout's "content" block;
// hello.html also re-emits the SIN-63294 /static/css/auth.css link
// through the new shell {{block "head_extra" .}} slot so the bare
// form/button baseline survives the migration.
//
// Data shape: struct { TenantName, UserID, CSRFToken,
// TenantThemeStyle, CSPNonce, Surfaces …, plus optional shell.Data
// fields (NavItems, UserMenuItems, UserDisplayName, TenantLogo) }.
// Legacy callers without the shell fields still render: every
// reflection helper in internal/web/shell falls back safely when the
// field is absent.
var Hello = shell.MustParse(csrfFuncs, assets, "hello.html")
