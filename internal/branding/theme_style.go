package branding

import (
	"context"
	"html/template"
)

// DefaultPalette is the deterministic neutral palette used when no
// per-tenant palette has been configured (or the lookup transiently
// fails). The numbers track the GitHub-flavoured "primer" defaults
// already baked into the existing static CSS so the visual output is
// unchanged for tenants without custom branding (AC #2 of SIN-63085).
//
// Treat the value as read-only. Source is fixed to PaletteSourceFallback
// so producers can distinguish the default from an Extracted result
// without recomputing.
var DefaultPalette = Palette{
	Primary:       RGB{R: 0x1f, G: 0x6f, B: 0xeb},
	Secondary:     RGB{R: 0x57, G: 0x60, B: 0x6a},
	Accent:        RGB{R: 0x54, G: 0xae, B: 0xff},
	Foreground:    RGB{R: 0x1f, G: 0x23, B: 0x28},
	Background:    RGB{R: 0xff, G: 0xff, B: 0xff},
	TextOnPrimary: RGB{R: 0xff, G: 0xff, B: 0xff},
	Source:        PaletteSourceFallback,
}

// DefaultThemeStyle is the pre-rendered :root{...} declaration block
// for DefaultPalette. Computed once at package init so the hot path
// (every unbranded request) is allocation-free.
var DefaultThemeStyle = themeStyleFor(DefaultPalette)

// ThemeStyleFromPalette wraps the inline-style declarations of p in a
// `:root{...}` selector so the value can be dropped directly into a
// `<style>` element. The returned value is typed template.CSS so
// html/template does not double-escape the hex/colon/semicolon glyphs
// when the layout interpolates it.
//
// The output is deterministic and stable (the token order is fixed
// inside ThemeTokens.RenderInlineStyle), making it safe to snapshot in
// regression tests.
func ThemeStyleFromPalette(p Palette) template.CSS {
	return themeStyleFor(p)
}

func themeStyleFor(p Palette) template.CSS {
	return template.CSS(":root{" + ThemeTokensFromPalette(p).RenderInlineStyle() + "}")
}

type themeStyleCtxKey struct{}

// WithThemeStyle returns a child context carrying style for the
// downstream handlers. The theme middleware in
// internal/adapter/httpapi/middleware calls this after resolving the
// per-tenant palette; handlers read it back with ThemeStyleFromContext
// and pipe the value into their template data.
func WithThemeStyle(ctx context.Context, style template.CSS) context.Context {
	return context.WithValue(ctx, themeStyleCtxKey{}, style)
}

// ThemeStyleFromContext returns the per-request inline style attached
// by the theme middleware. When the middleware has not been mounted
// (or an upstream layer cleared the context) the returned value is
// DefaultThemeStyle so handlers can pass the result into their template
// data unconditionally — no nil branch in the hot path.
func ThemeStyleFromContext(ctx context.Context) template.CSS {
	if v, ok := ctx.Value(themeStyleCtxKey{}).(template.CSS); ok && v != "" {
		return v
	}
	return DefaultThemeStyle
}
