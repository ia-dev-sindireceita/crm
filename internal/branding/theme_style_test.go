package branding_test

import (
	"context"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestDefaultThemeStyle_ShapeIsStable pins the rendered :root{...} body
// of DefaultThemeStyle: token order, hex casing, and trailing brace.
// The layout template (and downstream snapshot tests) rely on the
// exact byte sequence — a refactor that reorders ThemeTokens or drops
// the :root wrapper would silently change UI for every unbranded
// tenant.
func TestDefaultThemeStyle_ShapeIsStable(t *testing.T) {
	t.Parallel()
	got := string(branding.DefaultThemeStyle)
	want := ":root{" +
		"--color-primary:#1f6feb;" +
		"--color-secondary:#57606a;" +
		"--color-accent:#54aeff;" +
		"--color-foreground:#1f2328;" +
		"--color-background:#ffffff;" +
		"--color-text-on-primary:#ffffff" +
		"}"
	if got != want {
		t.Fatalf("DefaultThemeStyle drift\n got: %q\nwant: %q", got, want)
	}
}

func TestDefaultPalette_SourceIsFallback(t *testing.T) {
	t.Parallel()
	if got := branding.DefaultPalette.Source; got != branding.PaletteSourceFallback {
		t.Fatalf("Source = %v, want PaletteSourceFallback", got)
	}
}

// TestThemeStyleFromPalette_RoundTrip asserts the helper renders an
// arbitrary palette into the same :root{...} shape as the default,
// using the palette's colours. Used by the middleware to materialise
// per-tenant overrides.
func TestThemeStyleFromPalette_RoundTrip(t *testing.T) {
	t.Parallel()
	p := branding.Palette{
		Primary:       branding.RGB{R: 0xaa, G: 0xbb, B: 0xcc},
		Secondary:     branding.RGB{R: 0x11, G: 0x22, B: 0x33},
		Accent:        branding.RGB{R: 0x44, G: 0x55, B: 0x66},
		Foreground:    branding.RGB{R: 0x77, G: 0x88, B: 0x99},
		Background:    branding.RGB{R: 0xcc, G: 0xdd, B: 0xee},
		TextOnPrimary: branding.RGB{R: 0xff, G: 0xff, B: 0xff},
	}
	got := string(branding.ThemeStyleFromPalette(p))
	if !strings.HasPrefix(got, ":root{--color-primary:#aabbcc;") {
		t.Fatalf("unexpected prefix: %q", got)
	}
	if !strings.HasSuffix(got, "--color-text-on-primary:#ffffff}") {
		t.Fatalf("unexpected suffix: %q", got)
	}
}

func TestThemeStyleFromContext_DefaultWhenMissing(t *testing.T) {
	t.Parallel()
	if got := branding.ThemeStyleFromContext(context.Background()); got != branding.DefaultThemeStyle {
		t.Fatalf("missing context did not return DefaultThemeStyle: %q", got)
	}
}

func TestThemeStyleFromContext_RoundTrip(t *testing.T) {
	t.Parallel()
	want := template.CSS(":root{--color-primary:#ababab}")
	ctx := branding.WithThemeStyle(context.Background(), want)
	if got := branding.ThemeStyleFromContext(ctx); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestThemeStyleFromContext_EmptyValueFallsBack guards the
// branding.WithThemeStyle("") edge case: an empty string is treated
// the same as missing, so a partially-wired adapter cannot poison
// downstream handlers into rendering an empty <style> body.
func TestThemeStyleFromContext_EmptyValueFallsBack(t *testing.T) {
	t.Parallel()
	ctx := branding.WithThemeStyle(context.Background(), template.CSS(""))
	if got := branding.ThemeStyleFromContext(ctx); got != branding.DefaultThemeStyle {
		t.Fatalf("empty stored value should fall back to default, got %q", got)
	}
}

// TestThemeStyleFromContext_WrongTypeFallsBack pins the type-assert
// guard: a misuse that stuffed a non-template.CSS value into the
// reserved context key must not crash the lookup.
func TestThemeStyleFromContext_WrongTypeFallsBack(t *testing.T) {
	t.Parallel()
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, "not the theme key")
	if got := branding.ThemeStyleFromContext(ctx); got != branding.DefaultThemeStyle {
		t.Fatalf("expected DefaultThemeStyle, got %q", got)
	}
}
