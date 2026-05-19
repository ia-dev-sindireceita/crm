package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

func TestNewPaletteExtractor_RoundtripPalette(t *testing.T) {
	t.Parallel()
	ex := newPaletteExtractor(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	if ex == nil {
		t.Fatal("newPaletteExtractor returned nil")
	}

	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		c := color.NRGBA{R: 0xD8, G: 0x1F, B: 0x1F, A: 0xFF}
		if y >= 16 {
			c = color.NRGBA{R: 0x1E, G: 0x40, B: 0xAF, A: 0xFF}
		}
		for x := 0; x < 32; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	pal, err := ex.Extract(context.Background(), &buf, branding.Hint{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if cr := pal.TextOnPrimary.Contrast(pal.Primary); cr < branding.WCAGAANormalText {
		t.Fatalf("contrast = %.2f, want ≥ 4.5", cr)
	}
}

func TestNewPaletteExtractor_NilLoggerIsSafe(t *testing.T) {
	t.Parallel()
	if ex := newPaletteExtractor(nil); ex == nil {
		t.Fatal("nil logger should produce a working extractor")
	}
}
