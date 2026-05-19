package branding

import (
	"strings"
	"testing"
)

func TestThemeTokensFromPalette_Roundtrip(t *testing.T) {
	t.Parallel()

	p := Palette{
		Primary:       RGB{R: 0x1F, G: 0x29, B: 0x37},
		Secondary:     RGB{R: 0x3B, G: 0x82, B: 0xF6},
		Accent:        RGB{R: 0xF5, G: 0x9E, B: 0x0B},
		Foreground:    RGB{R: 0x0F, G: 0x11, B: 0x15},
		Background:    RGB{R: 0xFF, G: 0xFF, B: 0xFF},
		TextOnPrimary: RGB{R: 0xFF, G: 0xFF, B: 0xFF},
		Source:        PaletteSourceExtracted,
	}
	tokens := ThemeTokensFromPalette(p)

	if tokens.Primary != p.Primary ||
		tokens.Secondary != p.Secondary ||
		tokens.Accent != p.Accent ||
		tokens.Foreground != p.Foreground ||
		tokens.Background != p.Background ||
		tokens.TextOnPrimary != p.TextOnPrimary {
		t.Fatalf("ThemeTokensFromPalette dropped a slot: got %+v from %+v", tokens, p)
	}
}

func TestThemeTokens_RenderInlineStyle(t *testing.T) {
	t.Parallel()

	tokens := ThemeTokens{
		Primary:       RGB{R: 0x1F, G: 0x29, B: 0x37},
		Secondary:     RGB{R: 0x3B, G: 0x82, B: 0xF6},
		Accent:        RGB{R: 0xF5, G: 0x9E, B: 0x0B},
		Foreground:    RGB{R: 0x0F, G: 0x11, B: 0x15},
		Background:    RGB{R: 0xFF, G: 0xFF, B: 0xFF},
		TextOnPrimary: RGB{R: 0xFF, G: 0xFF, B: 0xFF},
	}

	want := strings.Join([]string{
		"--color-primary:#1f2937",
		"--color-secondary:#3b82f6",
		"--color-accent:#f59e0b",
		"--color-foreground:#0f1115",
		"--color-background:#ffffff",
		"--color-text-on-primary:#ffffff",
	}, ";")

	if got := tokens.RenderInlineStyle(); got != want {
		t.Fatalf("RenderInlineStyle()\n got: %s\nwant: %s", got, want)
	}
}

func TestThemeTokens_RenderInlineStyle_Deterministic(t *testing.T) {
	t.Parallel()

	tokens := ThemeTokens{Primary: RGB{R: 1, G: 2, B: 3}}
	first := tokens.RenderInlineStyle()
	for i := 0; i < 8; i++ {
		if got := tokens.RenderInlineStyle(); got != first {
			t.Fatalf("RenderInlineStyle drifted on call %d: %q vs %q", i, got, first)
		}
	}
}
