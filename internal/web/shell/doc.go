// Package shell is the SIN-63935 (UX-F1) design-system foundation. It
// renders the application chrome — top-bar with tenant brand, primary
// nav, and user menu — and exposes the canonical html/template layout
// every authenticated feature mounts into.
//
// Feature packages (internal/web/inbox, internal/web/funnel, …) build
// their own page templates and call Parse to compose them onto the
// shell layout. Each page defines "content" (required) and may override
// "title" (optional, defaults to "CRM"). The shell layout pulls page
// data via a small reflection-based FuncMap so feature data structs
// stay decoupled from the chrome — they just need fields whose names
// match (TenantName, NavItems, UserMenuItems, CSRFToken, CSPNonce, …)
// or embed shell.Data.
//
// Static assets — tokens.css, components.css, app-shell.css — live in
// web/static/css/. The shell layout loads them in that order, so every
// downstream feature inherits the same token set without re-linking.
//
// Hexagonal boundary: shell depends only on html/template, the CSRF
// helper package (HTML-rendering helper, not transport), and stdlib.
// No imports from iam/branding/transport packages; role-based nav
// filtering happens in the caller before NavItems is passed in.
package shell
