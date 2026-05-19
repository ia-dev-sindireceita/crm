// Package branding defines the per-tenant visual identity domain: the
// extracted colour Palette, its CSS-variable projection ThemeTokens,
// and the PaletteExtractor port that derives a Palette from a tenant
// logo.
//
// The package is deliberately storage- and transport-agnostic: it
// imports nothing beyond the Go standard library, so any caller — a
// usecase, an HTTP handler, a worker — can wire it without dragging
// imaging libraries along. Concrete extraction is the responsibility
// of an adapter under internal/adapter/branding/{mediancut,…},
// selected at composition time. See ADR 0060 for the rationale and
// the WCAG AA contrast policy this port enforces.
//
// The package is split as follows:
//
//   - palette.go      — RGB, Palette, PaletteSource and colour helpers,
//   - theme_tokens.go — ThemeTokens projection used by the renderer,
//   - extractor.go    — PaletteExtractor port + Hint + error sentinels,
//   - wcag.go         — WCAG AA contrast computation and EnsureWCAGAA.
package branding
