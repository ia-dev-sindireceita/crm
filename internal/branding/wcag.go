package branding

import "math"

// WCAGAANormalText is the WCAG 2.x contrast threshold for normal text
// (Success Criterion 1.4.3). EnsureWCAGAA uses it as the floor for
// the Foreground/Background and TextOnPrimary/Primary pairs.
const WCAGAANormalText = 4.5

// Deterministic candidates used by EnsureWCAGAA. The values match
// ADR 0060 §"WCAG AA policy".
var (
	// wcagNearBlack is the dark text candidate (#0F1115).
	wcagNearBlack = RGB{R: 0x0F, G: 0x11, B: 0x15}
	// wcagWhite is the light text candidate and the v1 background.
	wcagWhite = RGB{R: 0xFF, G: 0xFF, B: 0xFF}
	// wcagFallbackPrimary is the deterministic Primary substitute
	// produced when neither text candidate clears the threshold even
	// after the HSL-darken loop.
	wcagFallbackPrimary = RGB{R: 0x1F, G: 0x29, B: 0x37}
)

// hslDarkenStep is the per-iteration lightness reduction in the
// ADR 0060 fallback loop.
const hslDarkenStep = 0.05

// hslDarkenMaxSteps caps the loop at 6 iterations before substituting
// the deterministic fallback pair (ADR 0060). It is a var rather than
// a const so the test suite can shrink it to exercise the fallback
// branch — the production value is the ADR-mandated 6.
var hslDarkenMaxSteps = 6

// Contrast returns the WCAG 2.x contrast ratio between c and other
// in the range [1.0, 21.0]. The relation is symmetric: c.Contrast(o)
// == o.Contrast(c).
func (c RGB) Contrast(other RGB) float64 {
	l1 := relativeLuminance(c)
	l2 := relativeLuminance(other)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

// EnsureWCAGAA returns a Palette whose Foreground/Background and
// TextOnPrimary/Primary pairs both clear WCAGAANormalText. The
// algorithm follows ADR 0060 §"WCAG AA policy" exactly:
//
//  1. Foreground is reassigned to whichever of nearBlack/white has
//     the highest contrast against the existing Background.
//  2. TextOnPrimary is reassigned to whichever of nearBlack/white has
//     the highest contrast against the existing Primary.
//  3. If the chosen TextOnPrimary still does not reach the threshold,
//     Primary is darkened in HSL by hslDarkenStep at most
//     hslDarkenMaxSteps times. TextOnPrimary is re-picked after each
//     step.
//  4. If the loop exhausts without clearing the threshold, Primary is
//     replaced with wcagFallbackPrimary, TextOnPrimary is set to
//     white, and Source is set to PaletteSourceFallback.
//
// The function never returns a non-nil error today — the algorithm
// is total — but the signature reserves the right to surface
// structural validation in the future. Callers should still check
// it.
func EnsureWCAGAA(p Palette) (Palette, error) {
	p.Foreground = pickHighContrast(p.Background)
	p.TextOnPrimary = pickHighContrast(p.Primary)

	if p.TextOnPrimary.Contrast(p.Primary) >= WCAGAANormalText {
		if p.Source == PaletteSourceUnknown {
			p.Source = PaletteSourceExtracted
		}
		return p, nil
	}

	h, s, l := rgbToHSL(p.Primary)
	for step := 1; step <= hslDarkenMaxSteps; step++ {
		l -= hslDarkenStep
		if l < 0 {
			l = 0
		}
		candidate := hslToRGB(h, s, l)
		text := pickHighContrast(candidate)
		if text.Contrast(candidate) >= WCAGAANormalText {
			p.Primary = candidate
			p.TextOnPrimary = text
			if p.Source == PaletteSourceUnknown {
				p.Source = PaletteSourceExtracted
			}
			return p, nil
		}
	}

	p.Primary = wcagFallbackPrimary
	p.TextOnPrimary = wcagWhite
	p.Source = PaletteSourceFallback
	return p, nil
}

// pickHighContrast returns whichever of wcagNearBlack/wcagWhite gives
// the higher contrast ratio against against. Ties resolve to
// wcagNearBlack so the output is deterministic.
func pickHighContrast(against RGB) RGB {
	if wcagNearBlack.Contrast(against) >= wcagWhite.Contrast(against) {
		return wcagNearBlack
	}
	return wcagWhite
}

func relativeLuminance(c RGB) float64 {
	return 0.2126*linearise(c.R) + 0.7152*linearise(c.G) + 0.0722*linearise(c.B)
}

func linearise(channel uint8) float64 {
	cf := float64(channel) / 255.0
	if cf <= 0.03928 {
		return cf / 12.92
	}
	return math.Pow((cf+0.055)/1.055, 2.4)
}

func rgbToHSL(c RGB) (h, s, l float64) {
	r := float64(c.R) / 255
	g := float64(c.G) / 255
	b := float64(c.B) / 255
	mx := math.Max(r, math.Max(g, b))
	mn := math.Min(r, math.Min(g, b))
	l = (mx + mn) / 2
	if mx == mn {
		return 0, 0, l
	}
	d := mx - mn
	if l > 0.5 {
		s = d / (2 - mx - mn)
	} else {
		s = d / (mx + mn)
	}
	switch mx {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h /= 6
	return h, s, l
}

func hslToRGB(h, s, l float64) RGB {
	if s == 0 {
		v := uint8(math.Round(l * 255))
		return RGB{R: v, G: v, B: v}
	}
	var q float64
	if l < 0.5 {
		q = l * (1 + s)
	} else {
		q = l + s - l*s
	}
	p := 2*l - q
	return RGB{
		R: uint8(math.Round(hueToRGB(p, q, h+1.0/3) * 255)),
		G: uint8(math.Round(hueToRGB(p, q, h) * 255)),
		B: uint8(math.Round(hueToRGB(p, q, h-1.0/3) * 255)),
	}
}

func hueToRGB(p, q, t float64) float64 {
	if t < 0 {
		t += 1
	}
	if t > 1 {
		t -= 1
	}
	switch {
	case t < 1.0/6:
		return p + (q-p)*6*t
	case t < 0.5:
		return q
	case t < 2.0/3:
		return p + (q-p)*(2.0/3-t)*6
	default:
		return p
	}
}
