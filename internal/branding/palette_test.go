package branding

import "testing"

func TestRGB_Hex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   RGB
		want string
	}{
		{"black", RGB{}, "#000000"},
		{"white", RGB{R: 0xFF, G: 0xFF, B: 0xFF}, "#ffffff"},
		{"single digits pad", RGB{R: 0x01, G: 0x02, B: 0x03}, "#010203"},
		{"mid grey", RGB{R: 0x7F, G: 0x7F, B: 0x7F}, "#7f7f7f"},
		{"tailwind slate-800", RGB{R: 0x1F, G: 0x29, B: 0x37}, "#1f2937"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.Hex(); got != tc.want {
				t.Fatalf("Hex() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPaletteSource_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   PaletteSource
		want string
	}{
		{PaletteSourceUnknown, "unknown"},
		{PaletteSourceExtracted, "extracted"},
		{PaletteSourceFallback, "fallback"},
		{PaletteSourceManual, "manual"},
		{PaletteSource(99), "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
