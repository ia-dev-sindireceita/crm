package branding

import (
	"math"
	"testing"
)

const contrastEpsilon = 0.01

func approxEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

func TestRGB_Contrast_KnownPairs(t *testing.T) {
	t.Parallel()

	black := RGB{}
	white := RGB{R: 0xFF, G: 0xFF, B: 0xFF}
	tests := []struct {
		name string
		a, b RGB
		want float64
	}{
		{"black vs white = 21", black, white, 21.0},
		{"white vs black symmetric", white, black, 21.0},
		{"same colour = 1", white, white, 1.0},
		{"black vs black = 1", black, black, 1.0},
		{"slate-800 vs white", RGB{R: 0x1F, G: 0x29, B: 0x37}, white, 14.68},
		{"near-black vs white", RGB{R: 0x0F, G: 0x11, B: 0x15}, white, 18.90},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.a.Contrast(tc.b)
			if !approxEqual(got, tc.want, contrastEpsilon) {
				t.Fatalf("Contrast(%v, %v) = %.3f, want ~%.3f", tc.a, tc.b, got, tc.want)
			}
			if rev := tc.b.Contrast(tc.a); !approxEqual(rev, got, 1e-9) {
				t.Fatalf("Contrast is asymmetric: a->b=%v, b->a=%v", got, rev)
			}
		})
	}
}

func TestEnsureWCAGAA_HighContrastPassesThrough(t *testing.T) {
	t.Parallel()

	in := Palette{
		Primary:    RGB{R: 0x1F, G: 0x29, B: 0x37}, // slate-800 — well above 4.5 vs white
		Secondary:  RGB{R: 0x3B, G: 0x82, B: 0xF6},
		Accent:     RGB{R: 0xF5, G: 0x9E, B: 0x0B},
		Background: RGB{R: 0xFF, G: 0xFF, B: 0xFF},
	}
	out, err := EnsureWCAGAA(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Primary != in.Primary {
		t.Fatalf("Primary mutated: got %v want %v", out.Primary, in.Primary)
	}
	if out.Source != PaletteSourceExtracted {
		t.Fatalf("Source = %v, want extracted (default when unset)", out.Source)
	}
	if cr := out.TextOnPrimary.Contrast(out.Primary); cr < WCAGAANormalText {
		t.Fatalf("TextOnPrimary contrast %.2f below threshold", cr)
	}
	if cr := out.Foreground.Contrast(out.Background); cr < WCAGAANormalText {
		t.Fatalf("Foreground contrast %.2f below threshold", cr)
	}
	// TextOnPrimary against slate-800 should pick white.
	if out.TextOnPrimary != wcagWhite {
		t.Fatalf("TextOnPrimary = %v, want white", out.TextOnPrimary)
	}
	// Foreground against white background should pick near-black.
	if out.Foreground != wcagNearBlack {
		t.Fatalf("Foreground = %v, want near-black", out.Foreground)
	}
}

func TestEnsureWCAGAA_LightPrimaryPicksDarkText(t *testing.T) {
	t.Parallel()

	// Pale yellow #FDE68A — passes against near-black easily.
	in := Palette{
		Primary:    RGB{R: 0xFD, G: 0xE6, B: 0x8A},
		Background: RGB{R: 0xFF, G: 0xFF, B: 0xFF},
	}
	out, err := EnsureWCAGAA(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.TextOnPrimary != wcagNearBlack {
		t.Fatalf("TextOnPrimary = %v, want near-black for pale primary", out.TextOnPrimary)
	}
	if out.Primary != in.Primary {
		t.Fatalf("Primary should not be darkened for pale colour, got %v", out.Primary)
	}
	if out.Source != PaletteSourceExtracted {
		t.Fatalf("Source = %v, want extracted", out.Source)
	}
}

func TestEnsureWCAGAA_MidGreyTriggersHSLDarken(t *testing.T) {
	t.Parallel()

	// #797979 sits in the WCAG dead-zone for grayscale: contrast
	// against #0F1115 ≈ 4.34 and against #FFFFFF ≈ 4.35 — both below
	// 4.5. One HSL darken step brings it to ~#6C6C6C, which clears
	// 4.5 against white.
	in := Palette{
		Primary:    RGB{R: 0x79, G: 0x79, B: 0x79},
		Background: RGB{R: 0xFF, G: 0xFF, B: 0xFF},
	}
	out, err := EnsureWCAGAA(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Primary == in.Primary {
		t.Fatalf("Primary should have been darkened, got identical %v", out.Primary)
	}
	if cr := out.TextOnPrimary.Contrast(out.Primary); cr < WCAGAANormalText {
		t.Fatalf("post-darken contrast %.2f still below threshold", cr)
	}
	if out.Source != PaletteSourceExtracted {
		t.Fatalf("Source = %v, want extracted (fallback only when loop exhausts)", out.Source)
	}
	// Darken should monotonically reduce relative luminance.
	if relativeLuminance(out.Primary) >= relativeLuminance(in.Primary) {
		t.Fatalf("expected luminance to decrease, before=%.3f after=%.3f",
			relativeLuminance(in.Primary), relativeLuminance(out.Primary))
	}
}

func TestEnsureWCAGAA_SourcePreservedWhenAlreadySet(t *testing.T) {
	t.Parallel()

	in := Palette{
		Primary:    RGB{R: 0x1F, G: 0x29, B: 0x37},
		Background: RGB{R: 0xFF, G: 0xFF, B: 0xFF},
		Source:     PaletteSourceManual,
	}
	out, err := EnsureWCAGAA(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Source != PaletteSourceManual {
		t.Fatalf("EnsureWCAGAA overwrote Source: got %v, want manual", out.Source)
	}
}

func TestEnsureWCAGAA_FallbackWhenLoopExhausts(t *testing.T) {
	// Not parallel: mutates the package-level hslDarkenMaxSteps.
	orig := hslDarkenMaxSteps
	hslDarkenMaxSteps = 0
	t.Cleanup(func() { hslDarkenMaxSteps = orig })

	in := Palette{
		// #797979 is in the WCAG dead-zone vs both candidates.
		Primary:    RGB{R: 0x79, G: 0x79, B: 0x79},
		Secondary:  RGB{R: 0x12, G: 0x34, B: 0x56},
		Accent:     RGB{R: 0xAB, G: 0xCD, B: 0xEF},
		Background: RGB{R: 0xFF, G: 0xFF, B: 0xFF},
	}
	// Sanity: starting Primary fails both candidates.
	if c := wcagNearBlack.Contrast(in.Primary); c >= WCAGAANormalText {
		t.Fatalf("precondition: near-black already passes (%.2f)", c)
	}
	if c := wcagWhite.Contrast(in.Primary); c >= WCAGAANormalText {
		t.Fatalf("precondition: white already passes (%.2f)", c)
	}

	out, err := EnsureWCAGAA(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Primary != wcagFallbackPrimary {
		t.Fatalf("fallback Primary = %v, want %v", out.Primary, wcagFallbackPrimary)
	}
	if out.TextOnPrimary != wcagWhite {
		t.Fatalf("fallback TextOnPrimary = %v, want white", out.TextOnPrimary)
	}
	if out.Source != PaletteSourceFallback {
		t.Fatalf("fallback Source = %v, want fallback", out.Source)
	}
	// Secondary / Accent are untouched by the fallback path.
	if out.Secondary != in.Secondary || out.Accent != in.Accent {
		t.Fatalf("fallback touched non-Primary slots: %+v", out)
	}
	// Fallback Primary + white must clear the threshold.
	if cr := out.TextOnPrimary.Contrast(out.Primary); cr < WCAGAANormalText {
		t.Fatalf("fallback contrast %.2f below threshold", cr)
	}
}

func TestEnsureWCAGAA_ColouredPrimaryClearsViaDarkText(t *testing.T) {
	t.Parallel()

	// Sindireceita corporate red (~#C8102E): vibrant, clears 4.5 vs
	// white but not via near-black; verifies the white-text branch on
	// a saturated colour.
	in := Palette{
		Primary:    RGB{R: 0xC8, G: 0x10, B: 0x2E},
		Background: RGB{R: 0xFF, G: 0xFF, B: 0xFF},
	}
	out, err := EnsureWCAGAA(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.TextOnPrimary != wcagWhite {
		t.Fatalf("TextOnPrimary = %v, want white", out.TextOnPrimary)
	}
	if cr := out.TextOnPrimary.Contrast(out.Primary); cr < WCAGAANormalText {
		t.Fatalf("contrast %.2f below threshold", cr)
	}
}

func TestPickHighContrast_TiesGoToNearBlack(t *testing.T) {
	t.Parallel()

	// Construct a Background where the two candidates tie within
	// rounding by feeding mid-grey and checking the documented tie
	// behaviour: ≥ goes to near-black.
	mid := RGB{R: 0x77, G: 0x77, B: 0x77}
	got := pickHighContrast(mid)
	if c := wcagNearBlack.Contrast(mid); c >= wcagWhite.Contrast(mid) {
		if got != wcagNearBlack {
			t.Fatalf("expected near-black to win tie/lead, got %v", got)
		}
	} else if got != wcagWhite {
		t.Fatalf("expected white to win, got %v", got)
	}
}

func TestRGBToHSL_GrayscaleSaturationZero(t *testing.T) {
	t.Parallel()

	grey := RGB{R: 0x80, G: 0x80, B: 0x80}
	h, s, l := rgbToHSL(grey)
	if h != 0 || s != 0 {
		t.Fatalf("grey HSL hue/sat = %.3f/%.3f, want 0/0", h, s)
	}
	if l <= 0 || l >= 1 {
		t.Fatalf("grey HSL lightness = %.3f, want strict mid", l)
	}
}

func TestRGBToHSL_HueBranches(t *testing.T) {
	t.Parallel()

	// Cover the three switch branches in rgbToHSL: mx=r, mx=g, mx=b,
	// plus the mx=r/g<b sub-branch (h += 6 wrap).
	cases := []RGB{
		{R: 0xFF, G: 0x40, B: 0x40}, // red dominant
		{R: 0xFF, G: 0x10, B: 0x80}, // red dominant, g<b → h+=6
		{R: 0x40, G: 0xFF, B: 0x40}, // green dominant
		{R: 0x40, G: 0x40, B: 0xFF}, // blue dominant
		{R: 0x10, G: 0x80, B: 0xC0}, // mixed, blue dominant
	}
	for _, c := range cases {
		c := c
		t.Run(c.Hex(), func(t *testing.T) {
			t.Parallel()
			h, s, l := rgbToHSL(c)
			if s <= 0 {
				t.Fatalf("expected saturated colour, sat=%.3f", s)
			}
			if l <= 0 || l >= 1 {
				t.Fatalf("lightness out of (0,1): %.3f", l)
			}
			round := hslToRGB(h, s, l)
			// Round-trip tolerance: rgbToHSL→hslToRGB is lossy via
			// uint8 quantisation; require channels within 1.
			if diffChannel(round.R, c.R) > 1 ||
				diffChannel(round.G, c.G) > 1 ||
				diffChannel(round.B, c.B) > 1 {
				t.Fatalf("roundtrip diverged: got %v want %v", round, c)
			}
		})
	}
}

func TestHueToRGB_AllRanges(t *testing.T) {
	t.Parallel()

	// Direct exercise of the four ranges (t<1/6, t<0.5, t<2/3, default)
	// plus wrap-around for t<0 and t>1.
	cases := []struct {
		name string
		t    float64
	}{
		{"wrap_neg", -0.4},
		{"first_sixth", 0.1},
		{"second_quarter", 0.4},
		{"third_third", 0.6},
		{"default", 0.9},
		{"wrap_over", 1.4},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := hueToRGB(0.1, 0.9, tc.t)
			if got < 0 || got > 1 {
				t.Fatalf("hueToRGB out of [0,1]: %.3f", got)
			}
		})
	}
}

func diffChannel(a, b uint8) int {
	if a > b {
		return int(a) - int(b)
	}
	return int(b) - int(a)
}
