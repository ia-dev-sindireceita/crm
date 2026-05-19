package branding

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

func TestParseHex_Variants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		ok   bool
		want branding.RGB
	}{
		{"#1f2937", true, branding.RGB{R: 0x1f, G: 0x29, B: 0x37}},
		{"#FFFFFF", true, branding.RGB{R: 0xff, G: 0xff, B: 0xff}},
		{"#000000", true, branding.RGB{R: 0x00, G: 0x00, B: 0x00}},
		{"  #abcdef  ", true, branding.RGB{R: 0xab, G: 0xcd, B: 0xef}},
		{"#123", false, branding.RGB{}},
		{"1f2937", false, branding.RGB{}},
		{"#zzzzzz", false, branding.RGB{}},
		{"", false, branding.RGB{}},
		{"#1f29377", false, branding.RGB{}},
	}
	for _, tc := range cases {
		got, ok := parseHex(tc.in)
		if ok != tc.ok {
			t.Fatalf("parseHex(%q) ok=%v, want %v", tc.in, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Fatalf("parseHex(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestSlotIsValid(t *testing.T) {
	t.Parallel()
	for _, s := range paletteSlots {
		if !slotIsValid(s) {
			t.Fatalf("slotIsValid(%q) = false", s)
		}
	}
	if slotIsValid("nope") {
		t.Fatalf("slotIsValid(\"nope\") = true")
	}
}

func TestHumanSlot_All(t *testing.T) {
	t.Parallel()
	pairs := map[string]string{
		"primary":         "primária",
		"secondary":       "secundária",
		"accent":          "destaque",
		"foreground":      "texto",
		"background":      "fundo",
		"text_on_primary": "texto sobre primária",
		"unknown":         "unknown",
	}
	for k, want := range pairs {
		if got := humanSlot(k); got != want {
			t.Fatalf("humanSlot(%q) = %q, want %q", k, got, want)
		}
	}
}

func TestParsePalette_MissingSlot_ReturnsError(t *testing.T) {
	t.Parallel()
	v := url.Values{}
	v.Set("primary", "#000000")
	r := httptest.NewRequest("POST", "/x", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	if _, err := parsePalette(r); err == nil {
		t.Fatalf("expected error for missing slot")
	}
}

func TestParsePalette_BadHex_ReturnsError(t *testing.T) {
	t.Parallel()
	v := url.Values{}
	for _, s := range paletteSlots {
		v.Set(s, "#abcdef")
	}
	v.Set("accent", "not-hex")
	r := httptest.NewRequest("POST", "/x", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	if _, err := parsePalette(r); err == nil {
		t.Fatalf("expected error for malformed hex")
	}
}

func TestValidatePaletteContrast_LowTextOnPrimary_Fails(t *testing.T) {
	t.Parallel()
	p := branding.Palette{
		Primary:       branding.RGB{R: 0x80, G: 0x80, B: 0x80},
		TextOnPrimary: branding.RGB{R: 0x80, G: 0x80, B: 0x80},
		Foreground:    branding.RGB{R: 0x00, G: 0x00, B: 0x00},
		Background:    branding.RGB{R: 0xff, G: 0xff, B: 0xff},
	}
	msg, ok := validatePaletteContrast(p)
	if ok {
		t.Fatalf("expected validation failure")
	}
	if !strings.Contains(msg, "primária") {
		t.Fatalf("message=%q, want to mention primária", msg)
	}
}

func TestValidatePaletteContrast_LowForegroundBackground_Fails(t *testing.T) {
	t.Parallel()
	p := branding.Palette{
		Primary:       branding.RGB{R: 0x00, G: 0x00, B: 0x00},
		TextOnPrimary: branding.RGB{R: 0xff, G: 0xff, B: 0xff},
		Foreground:    branding.RGB{R: 0xee, G: 0xee, B: 0xee},
		Background:    branding.RGB{R: 0xff, G: 0xff, B: 0xff},
	}
	msg, ok := validatePaletteContrast(p)
	if ok {
		t.Fatalf("expected validation failure")
	}
	if !strings.Contains(msg, "fundo") {
		t.Fatalf("message=%q, want to mention fundo", msg)
	}
}

func TestValidatePaletteContrast_GoodPair_Passes(t *testing.T) {
	t.Parallel()
	p := branding.Palette{
		Primary:       branding.RGB{R: 0x00, G: 0x00, B: 0x00},
		TextOnPrimary: branding.RGB{R: 0xff, G: 0xff, B: 0xff},
		Foreground:    branding.RGB{R: 0x00, G: 0x00, B: 0x00},
		Background:    branding.RGB{R: 0xff, G: 0xff, B: 0xff},
	}
	if msg, ok := validatePaletteContrast(p); !ok {
		t.Fatalf("expected pass, got %q", msg)
	}
}
