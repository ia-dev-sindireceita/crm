package branding

import "strings"

// ThemeTokens projects a Palette into the CSS custom properties
// consumed by the runtime theming layer (HTMX swaps include the
// tokens via an inline style on the document root). Producers build
// it via ThemeTokensFromPalette and emit it with RenderInlineStyle.
//
// Token names are stable and form part of the UI contract; renaming
// them is a breaking change for tenant-authored CSS overrides.
type ThemeTokens struct {
	Primary       RGB
	Secondary     RGB
	Accent        RGB
	Foreground    RGB
	Background    RGB
	TextOnPrimary RGB
}

// ThemeTokensFromPalette derives the CSS token bundle from a Palette.
// The mapping is structural — no normalisation, no contrast checking
// (that is EnsureWCAGAA's job before this is called).
func ThemeTokensFromPalette(p Palette) ThemeTokens {
	return ThemeTokens{
		Primary:       p.Primary,
		Secondary:     p.Secondary,
		Accent:        p.Accent,
		Foreground:    p.Foreground,
		Background:    p.Background,
		TextOnPrimary: p.TextOnPrimary,
	}
}

// RenderInlineStyle returns the tokens as a semicolon-separated
// declaration list suitable for the value of an HTML style attribute
// (e.g. on <html> or a per-tenant root element). Output is
// deterministic and ordered to make snapshot tests stable.
//
// Example:
//
//	--color-primary:#1f2937;--color-secondary:#3b82f6;...
//
// The caller is responsible for HTML-attribute escaping if the value
// ever flows through a templating context that does not auto-escape
// style attributes; the produced string contains only ASCII hex
// digits, hyphens, colons and semicolons.
func (t ThemeTokens) RenderInlineStyle() string {
	pairs := [...]struct {
		name string
		c    RGB
	}{
		{"--color-primary", t.Primary},
		{"--color-secondary", t.Secondary},
		{"--color-accent", t.Accent},
		{"--color-foreground", t.Foreground},
		{"--color-background", t.Background},
		{"--color-text-on-primary", t.TextOnPrimary},
	}
	var b strings.Builder
	b.Grow(len(pairs) * 32)
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(p.name)
		b.WriteByte(':')
		b.WriteString(p.c.Hex())
	}
	return b.String()
}
