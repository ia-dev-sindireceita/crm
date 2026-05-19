// Package mediancut implements branding.PaletteExtractor on top of
// github.com/cenkalti/dominantcolor, per ADR 0060. The package name
// follows the ADR's algorithm class — dominantcolor markets itself as
// median-cut though the current implementation is fixed-seed k-means.
// Determinism is what the ADR requires (so "revert to default palette"
// is well-defined and tests can pin RGB triples); both algorithms
// satisfy that with the pinned seed.
package mediancut

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg" // register decoder
	_ "image/png"  // register decoder
	"io"
	"log/slog"
	"math"
	"time"

	"github.com/cenkalti/dominantcolor"
	_ "golang.org/x/image/webp" // register decoder

	"github.com/pericles-luz/crm/internal/branding"
)

// Tunables fixed by ADR 0060. Kept as untyped consts (no per-instance
// option) — changing any of these is an ADR amendment, not a deployment
// knob.
const (
	defaultMaxBytes        = 2 << 20 // ADR 0080 §5 tenant-logo cap.
	clusters               = 5       // ADR 0060 §"Decision".
	neutralEpsilon         = 0.04    // ADR 0060 §"WCAG AA policy" step 2.
	minHueDistance         = 30.0    // ADR 0060 §"WCAG AA policy" step 3.
	alphaCutoff     uint32 = 16      // ADR 0060 §"Sizing and timing" step 3.
)

// Extractor implements branding.PaletteExtractor. Stateless and
// goroutine-safe — share a single instance per process.
type Extractor struct {
	logger *slog.Logger
}

// Option mutates an Extractor on construction.
type Option func(*Extractor)

// WithLogger overrides the default discard logger. A nil logger is
// ignored so callers don't need a guard at the call site.
func WithLogger(l *slog.Logger) Option {
	return func(e *Extractor) {
		if l != nil {
			e.logger = l
		}
	}
}

// New returns an Extractor satisfying branding.PaletteExtractor.
func New(opts ...Option) *Extractor {
	e := &Extractor{logger: slog.New(discardHandler{})}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// compile-time assertion
var _ branding.PaletteExtractor = (*Extractor)(nil)

// Extract decodes src, derives a five-slot palette via dominantcolor.FindN,
// applies the WCAG AA policy from ADR 0060, and returns the result.
// Errors wrap branding's sentinels (ErrInvalidImage, ErrTooLarge,
// ErrUnsupportedFormat) so callers can branch via errors.Is.
func (e *Extractor) Extract(ctx context.Context, src io.Reader, hint branding.Hint) (branding.Palette, error) {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return branding.Palette{}, err
	}

	maxBytes := hint.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	raw, err := io.ReadAll(io.LimitReader(src, maxBytes+1))
	if err != nil {
		return branding.Palette{}, fmt.Errorf("mediancut: read source: %w", branding.ErrInvalidImage)
	}
	byteSize := int64(len(raw))
	switch {
	case byteSize == 0:
		return branding.Palette{}, fmt.Errorf("mediancut: empty source: %w", branding.ErrInvalidImage)
	case byteSize > maxBytes:
		return branding.Palette{}, fmt.Errorf("mediancut: source exceeds %d bytes: %w", maxBytes, branding.ErrTooLarge)
	}
	if err := ctx.Err(); err != nil {
		return branding.Palette{}, err
	}

	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		if errors.Is(err, image.ErrFormat) {
			return branding.Palette{}, fmt.Errorf("mediancut: unrecognized format: %w", branding.ErrUnsupportedFormat)
		}
		return branding.Palette{}, fmt.Errorf("mediancut: decode %q: %w", format, branding.ErrInvalidImage)
	}
	if err := ctx.Err(); err != nil {
		return branding.Palette{}, err
	}

	candidates := toRGBs(dominantcolor.FindN(maskAlpha(img, alphaCutoff), clusters))
	if len(candidates) == 0 {
		// Pure-transparent input or zero-area image: hand EnsureWCAGAA a
		// mid-grey Primary so the deterministic fallback path fires.
		candidates = []branding.RGB{{R: 0x7F, G: 0x7F, B: 0x7F}}
	}
	primary, secondary, accent := slotPalette(candidates)

	palette := branding.Palette{
		Primary:    primary,
		Secondary:  secondary,
		Accent:     accent,
		Background: branding.RGB{R: 0xFF, G: 0xFF, B: 0xFF}, // ADR 0060 §7 — light-mode v1.
	}
	palette, err = branding.EnsureWCAGAA(palette)
	if err != nil {
		return branding.Palette{}, fmt.Errorf("mediancut: wcag: %w", err)
	}

	e.logger.Info("branding: palette extracted",
		"format", format,
		"byte_size", byteSize,
		"palette_source", palette.Source.String(),
		"primary", palette.Primary.Hex(),
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return palette, nil
}

// maskAlpha binarises the alpha channel: pixels with alpha < cutoff
// become fully transparent (dominantcolor.FindN's a==0 skip drops them);
// surviving pixels are emitted opaque with their un-premultiplied RGB so
// border antialiasing does not dim the cluster centroids toward the
// background blend.
func maskAlpha(src image.Image, cutoff uint32) image.Image {
	cutoff16 := cutoff << 8 // 0..255 → 0..0xFF00 in the 16-bit space.
	bounds := src.Bounds()
	dst := image.NewNRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			ri, gi, bi, ai := src.At(x, y).RGBA()
			if ai < cutoff16 {
				continue
			}
			dst.SetNRGBA(x, y, color.NRGBA{
				R: uint8(ri * 0xff / ai),
				G: uint8(gi * 0xff / ai),
				B: uint8(bi * 0xff / ai),
				A: 0xff,
			})
		}
	}
	return dst
}

// slotPalette assigns Primary/Secondary/Accent from candidates (sorted
// by dominance) per ADR 0060 §"WCAG AA policy" steps 2–4.
func slotPalette(candidates []branding.RGB) (primary, secondary, accent branding.RGB) {
	primaryIdx := 0
	for i, c := range candidates {
		if !isNeutral(c) {
			primaryIdx = i
			break
		}
	}
	primary = candidates[primaryIdx]

	secondary = primary
	for i, c := range candidates {
		if i == primaryIdx {
			continue
		}
		if hueDistance(primary, c) > minHueDistance {
			secondary = c
			break
		}
	}
	if secondary == primary {
		for i, c := range candidates {
			if i != primaryIdx {
				secondary = c
				break
			}
		}
	}

	accent = primary
	bestSat := -1.0
	for i, c := range candidates {
		if i == primaryIdx {
			continue
		}
		_, s, _ := rgbToHSL(c)
		if s > bestSat {
			bestSat = s
			accent = c
		}
	}
	return primary, secondary, accent
}

// isNeutral reports whether c sits within neutralEpsilon (L2 in the unit
// sRGB cube) of #000000 or #FFFFFF.
func isNeutral(c branding.RGB) bool {
	r := float64(c.R) / 255
	g := float64(c.G) / 255
	b := float64(c.B) / 255
	const norm = 1.7320508075688772 // sqrt(3) — diagonal of the unit cube.
	dBlack := math.Sqrt(r*r+g*g+b*b) / norm
	dWhite := math.Sqrt((1-r)*(1-r)+(1-g)*(1-g)+(1-b)*(1-b)) / norm
	return dBlack < neutralEpsilon || dWhite < neutralEpsilon
}

func hueDistance(a, b branding.RGB) float64 {
	ha, _, _ := rgbToHSL(a)
	hb, _, _ := rgbToHSL(b)
	diff := math.Abs(ha-hb) * 360
	if diff > 180 {
		diff = 360 - diff
	}
	return diff
}

// rgbToHSL — h in [0,1), s/l in [0,1]. Duplicated from internal/branding
// (the helper is unexported there) to keep the adapter from reaching into
// the domain's internals.
func rgbToHSL(c branding.RGB) (h, s, l float64) {
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

func toRGBs(raws []color.RGBA) []branding.RGB {
	out := make([]branding.RGB, 0, len(raws))
	for _, c := range raws {
		out = append(out, branding.RGB{R: c.R, G: c.G, B: c.B})
	}
	return out
}

// discardHandler is the slog.Handler equivalent of io.Discard. Tests
// substitute via WithLogger to capture structured records.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }
