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

	csrfhelpers "github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
)

//go:embed *.html
var assets embed.FS

// csrfFuncs exposes the CSRF templ helpers as html/template FuncMap
// entries (ADR 0073 §D1). The functions are pure — no request state —
// so wiring them at parse time is safe across goroutines. Each helper
// returns a html/template-safe type (template.HTML / template.HTMLAttr)
// so the {{}} interpolation does not double-escape the already-escaped
// payload.
var csrfFuncs = template.FuncMap{
	"csrfMeta":       csrfhelpers.MetaTag,
	"csrfHXHeaders":  csrfhelpers.HXHeadersAttr,
	"csrfFormHidden": csrfhelpers.FormHidden,
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
