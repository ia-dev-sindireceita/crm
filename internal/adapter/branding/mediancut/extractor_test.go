package mediancut_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/branding/mediancut"
	"github.com/pericles-luz/crm/internal/branding"
)

// Fixtures cover the seven representative cases from ADR 0060 §"Test
// fixtures". They are generated in-memory rather than checked in as PNG
// blobs so the diff stays reviewable and reproducible — the bytes still
// flow through image.Decode end-to-end.
//
// Note on Source expectations: EnsureWCAGAA's bounded HSL-darken loop
// rescues almost every real input from PaletteSourceFallback (6 steps ×
// ΔL = 0.30 is large enough to clear 4.5:1 against either text candidate
// for any non-pathological starting colour). The fallback branch is
// covered exhaustively in the branding package's own wcag_test.go with
// the test-only hslDarkenMaxSteps knob. Here we assert the contract that
// matters for the adapter: every fixture produces a palette satisfying
// WCAG AA, regardless of which branch EnsureWCAGAA took to get there.
func buildFixtures(t testing.TB) []struct {
	name  string
	bytes []byte
} {
	t.Helper()
	return []struct {
		name  string
		bytes []byte
	}{
		{name: "high_variance_brand", bytes: pngStripes(64, color.NRGBA{0xD8, 0x1F, 0x1F, 0xFF}, color.NRGBA{0xF7, 0x9A, 0x1E, 0xFF}, color.NRGBA{0xF6, 0xD2, 0x1A, 0xFF})},
		{name: "single_navy_on_transparent", bytes: pngOnTransparent(64, color.NRGBA{0x10, 0x1F, 0x4E, 0xFF})},
		{name: "monochrome_black_on_white", bytes: pngCentredDisc(64, color.NRGBA{0x00, 0x00, 0x00, 0xFF}, color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF})},
		{name: "pastel_low_contrast", bytes: pngStripes(64, color.NRGBA{0xC9, 0xE2, 0xF5, 0xFF}, color.NRGBA{0xD8, 0xEE, 0xC4, 0xFF}, color.NRGBA{0xF6, 0xE3, 0xCA, 0xFF})},
		{name: "mid_grey_solid", bytes: pngSolid(64, color.NRGBA{0x77, 0x77, 0x77, 0xFF})},
		{name: "alpha_gradient_blue", bytes: pngAlphaGradient(64, color.NRGBA{0x2B, 0x6E, 0xDD, 0xFF})},
		{name: "tiny_icon_32px", bytes: pngStripes(32, color.NRGBA{0x0E, 0xA5, 0xE9, 0xFF}, color.NRGBA{0xFA, 0xCC, 0x15, 0xFF}, color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF})},
		// 512-px fixture exercises the resampleNearest downsample branch
		// (input above resampleMax = 256 triggers the NearestNeighbor pass).
		{name: "large_512px_stripes", bytes: pngStripes(512, color.NRGBA{0xE0, 0x21, 0x21, 0xFF}, color.NRGBA{0x21, 0x21, 0xE0, 0xFF})},
	}
}

func TestExtract_FixtureMatrix(t *testing.T) {
	t.Parallel()
	ex := mediancut.New()
	ctx := context.Background()
	for _, f := range buildFixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			pal, err := ex.Extract(ctx, bytes.NewReader(f.bytes), branding.Hint{})
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if pal.Background != (branding.RGB{R: 0xFF, G: 0xFF, B: 0xFF}) {
				t.Fatalf("Background = %s, want #ffffff (ADR 0060 §7)", pal.Background.Hex())
			}
			if cr := pal.Foreground.Contrast(pal.Background); cr < branding.WCAGAANormalText {
				t.Fatalf("Foreground/Background contrast = %.2f, want ≥ 4.5", cr)
			}
			if cr := pal.TextOnPrimary.Contrast(pal.Primary); cr < branding.WCAGAANormalText {
				t.Fatalf("TextOnPrimary/Primary contrast = %.2f, want ≥ 4.5", cr)
			}
			if pal.Source != branding.PaletteSourceExtracted && pal.Source != branding.PaletteSourceFallback {
				t.Fatalf("Source = %s, want extracted or fallback", pal.Source)
			}
		})
	}
}

func TestExtract_JPEGFixture(t *testing.T) {
	t.Parallel()
	raw := jpegStripes(96, color.NRGBA{0xE0, 0x21, 0x21, 0xFF}, color.NRGBA{0xF7, 0xA0, 0x1A, 0xFF})
	pal, err := mediancut.New().Extract(context.Background(), bytes.NewReader(raw), branding.Hint{ContentType: "image/jpeg"})
	if err != nil {
		t.Fatalf("Extract jpeg: %v", err)
	}
	if cr := pal.TextOnPrimary.Contrast(pal.Primary); cr < branding.WCAGAANormalText {
		t.Fatalf("WCAG AA failed: %.2f", cr)
	}
}

func TestExtract_Determinism(t *testing.T) {
	t.Parallel()
	raw := pngStripes(64, color.NRGBA{0x33, 0x66, 0xCC, 0xFF}, color.NRGBA{0xCC, 0x33, 0x66, 0xFF}, color.NRGBA{0x66, 0xCC, 0x33, 0xFF})
	a, err := mediancut.New().Extract(context.Background(), bytes.NewReader(raw), branding.Hint{})
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := mediancut.New().Extract(context.Background(), bytes.NewReader(raw), branding.Hint{})
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a != b {
		t.Fatalf("non-deterministic: %+v vs %+v", a, b)
	}
}

func TestExtract_ErrorPaths(t *testing.T) {
	t.Parallel()
	ex := mediancut.New()
	ctx := context.Background()

	t.Run("empty", func(t *testing.T) {
		_, err := ex.Extract(ctx, bytes.NewReader(nil), branding.Hint{})
		if !errors.Is(err, branding.ErrInvalidImage) {
			t.Fatalf("err = %v, want wrapping ErrInvalidImage", err)
		}
	})

	t.Run("too_large", func(t *testing.T) {
		_, err := ex.Extract(ctx, bytes.NewReader(make([]byte, 32)), branding.Hint{MaxBytes: 8})
		if !errors.Is(err, branding.ErrTooLarge) {
			t.Fatalf("err = %v, want wrapping ErrTooLarge", err)
		}
	})

	t.Run("unsupported_format", func(t *testing.T) {
		_, err := ex.Extract(ctx, strings.NewReader("<svg xmlns='http://www.w3.org/2000/svg'/>"), branding.Hint{})
		if !errors.Is(err, branding.ErrUnsupportedFormat) {
			t.Fatalf("err = %v, want wrapping ErrUnsupportedFormat", err)
		}
	})

	t.Run("truncated_png", func(t *testing.T) {
		good := pngSolid(16, color.NRGBA{0x10, 0x20, 0x30, 0xFF})
		_, err := ex.Extract(ctx, bytes.NewReader(good[:len(good)-12]), branding.Hint{})
		if !errors.Is(err, branding.ErrInvalidImage) {
			t.Fatalf("err = %v, want wrapping ErrInvalidImage", err)
		}
	})

	t.Run("cancelled_ctx", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := ex.Extract(cctx, bytes.NewReader(pngSolid(8, color.NRGBA{1, 2, 3, 0xFF})), branding.Hint{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("read_error", func(t *testing.T) {
		_, err := ex.Extract(ctx, errReader{}, branding.Hint{})
		if !errors.Is(err, branding.ErrInvalidImage) {
			t.Fatalf("err = %v, want wrapping ErrInvalidImage", err)
		}
	})
}

func TestExtract_LoggerEmitsStructuredFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ex := mediancut.New(mediancut.WithLogger(logger))
	raw := pngSolid(32, color.NRGBA{0x21, 0x97, 0xF3, 0xFF})
	if _, err := ex.Extract(context.Background(), bytes.NewReader(raw), branding.Hint{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	out := buf.String()
	for _, want := range []string{`"format":"png"`, `"byte_size"`, `"duration_ms"`, `"palette_source"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q: %s", want, out)
		}
	}
}

func TestNew_WithLoggerNilIsNoop(t *testing.T) {
	t.Parallel()
	// Constructor must not panic when WithLogger(nil) is passed (defensive
	// no-op preserves the discard logger).
	ex := mediancut.New(mediancut.WithLogger(nil))
	raw := pngSolid(16, color.NRGBA{0x11, 0x22, 0x33, 0xFF})
	if _, err := ex.Extract(context.Background(), bytes.NewReader(raw), branding.Hint{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
}

func BenchmarkExtract_1024PNG(b *testing.B) {
	raw := pngStripes(1024, color.NRGBA{0xD8, 0x1F, 0x1F, 0xFF}, color.NRGBA{0xF7, 0x9A, 0x1E, 0xFF}, color.NRGBA{0xF6, 0xD2, 0x1A, 0xFF})
	ex := mediancut.New()
	ctx := context.Background()
	hint := branding.Hint{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ex.Extract(ctx, bytes.NewReader(raw), hint); err != nil {
			b.Fatal(err)
		}
	}
}

// --- fixture builders -------------------------------------------------------

func pngSolid(size int, c color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return encodePNG(img)
}

func pngStripes(size int, cs ...color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	stripeH := size / len(cs)
	for y := 0; y < size; y++ {
		idx := y / stripeH
		if idx >= len(cs) {
			idx = len(cs) - 1
		}
		for x := 0; x < size; x++ {
			img.SetNRGBA(x, y, cs[idx])
		}
	}
	return encodePNG(img)
}

func pngCentredDisc(size int, fg, bg color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	cx, cy := size/2, size/2
	r2 := (size / 3) * (size / 3)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r2 {
				img.SetNRGBA(x, y, fg)
			} else {
				img.SetNRGBA(x, y, bg)
			}
		}
	}
	return encodePNG(img)
}

func pngOnTransparent(size int, fg color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	band := size / 3
	for y := band; y < 2*band; y++ {
		for x := band / 2; x < size-band/2; x++ {
			img.SetNRGBA(x, y, fg)
		}
	}
	return encodePNG(img)
}

func pngAlphaGradient(size int, fg color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		// Alpha ramps from fully transparent at y=0 to fully opaque at y=size-1
		// so the lower half should survive the alpha cutoff and dominate the
		// palette.
		a := uint8((y * 0xFF) / (size - 1))
		px := fg
		px.A = a
		for x := 0; x < size; x++ {
			img.SetNRGBA(x, y, px)
		}
	}
	return encodePNG(img)
}

func jpegStripes(size int, cs ...color.NRGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	stripeH := size / len(cs)
	for y := 0; y < size; y++ {
		idx := y / stripeH
		if idx >= len(cs) {
			idx = len(cs) - 1
		}
		for x := 0; x < size; x++ {
			img.Set(x, y, cs[idx])
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
