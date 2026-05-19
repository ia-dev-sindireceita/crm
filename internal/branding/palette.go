package branding

import "fmt"

// RGB is an 8-bit-per-channel sRGB colour. Adapters returning a
// Palette MUST emit RGB values, not Go's image/color.Color, so the
// domain stays independent of the imaging stack.
type RGB struct {
	R, G, B uint8
}

// Hex returns the colour as a lowercase CSS hex literal ("#rrggbb").
// The format is stable across Go versions and is the canonical form
// consumed by ThemeTokens.RenderInlineStyle.
func (c RGB) Hex() string {
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

// PaletteSource records how the Palette was produced. Producers use
// it to surface UI affordances (e.g. "extracted from your logo" vs.
// "we couldn't derive a unique brand colour — adjust manually").
type PaletteSource uint8

const (
	// PaletteSourceUnknown is the zero value; never emitted by a
	// well-behaved adapter.
	PaletteSourceUnknown PaletteSource = iota
	// PaletteSourceExtracted means the Primary/Secondary/Accent slots
	// came from the logo without falling back to the neutral default.
	PaletteSourceExtracted
	// PaletteSourceFallback means the extractor could not derive a
	// palette satisfying WCAG AA from the logo and substituted the
	// deterministic neutral pair documented in ADR 0060.
	PaletteSourceFallback
	// PaletteSourceManual is reserved for future producer paths where
	// a human-edited palette bypasses extraction entirely. Adapters
	// MUST NOT emit it.
	PaletteSourceManual
)

// String renders the source as a stable identifier suitable for logs
// and metric labels.
func (s PaletteSource) String() string {
	switch s {
	case PaletteSourceExtracted:
		return "extracted"
	case PaletteSourceFallback:
		return "fallback"
	case PaletteSourceManual:
		return "manual"
	default:
		return "unknown"
	}
}

// Palette is the canonical five-slot tenant palette emitted by a
// PaletteExtractor. Foreground/Background and the implicit
// text-on-Primary pair are guaranteed by the adapter to meet WCAG AA
// (contrast ratio ≥ 4.5:1 for normal text); see ADR 0060.
//
// Producers MUST NOT recompute contrast at render time.
type Palette struct {
	Primary    RGB
	Secondary  RGB
	Accent     RGB
	Foreground RGB
	Background RGB

	// TextOnPrimary is the colour producers should use for text
	// rendered on a Primary plate (CTA labels, badges). It is one of
	// the two deterministic candidates documented in ADR 0060 and is
	// guaranteed to reach CR ≥ 4.5 against Primary.
	TextOnPrimary RGB

	Source PaletteSource
}
