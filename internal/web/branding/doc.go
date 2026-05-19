// Package branding serves the SIN-63084 HTMX admin surface that lets a
// tenant gerente upload a logo, preview the derived palette, manually
// override any of the six colour slots, and persist the result.
//
// Boundaries:
//
//   - The handler depends on the branding.PaletteExtractor port for
//     logo decoding (the SIN-63079 mediancut adapter is wired by
//     cmd/server) and on branding.PaletteStore + branding.PaletteWriter
//     for persistence (the SIN-63075 postgres adapter ships separately;
//     until then cmd/server wires the in-memory adapter in
//     internal/adapter/branding/memstore).
//   - The router mounts the routes behind RequireAuth +
//     RequireAction(iam.ActionTenantBrandingManage), so this package
//     never has to validate role or session — it trusts the upstream
//     middleware to filter requests.
//   - CSRF is honoured at the router boundary; this package reads the
//     session-bound token via the CSRFToken port and round-trips it
//     into every form / hx-headers attr so non-HTMX form submits work
//     without JS.
//
// Routes mounted:
//
//	GET   /branding                       — page (form + preview + upload)
//	POST  /branding/logo                  — multipart upload → preview
//	POST  /branding/palette/override      — single-slot hex override
//	POST  /branding/palette/save          — persist current palette
//	POST  /branding/palette/revert        — drop overrides → default
package branding
