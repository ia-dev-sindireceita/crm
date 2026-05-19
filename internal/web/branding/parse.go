package branding

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/pericles-luz/crm/internal/branding"
)

// parsePalette extracts a Palette from the round-tripped hidden form
// inputs. Every slot must be present and parseable; an empty or
// malformed value returns a descriptive error the caller surfaces as
// the inline 422 message.
func parsePalette(r *http.Request) (branding.Palette, error) {
	rgbs := make(map[string]branding.RGB, len(paletteSlots))
	for _, slot := range paletteSlots {
		raw := strings.ToLower(strings.TrimSpace(r.Form.Get(slot)))
		if raw == "" {
			return branding.Palette{}, fmt.Errorf("cor obrigatória ausente: %s", humanSlot(slot))
		}
		rgb, ok := parseHex(raw)
		if !ok {
			return branding.Palette{}, fmt.Errorf("cor inválida em %s (use #RRGGBB)", humanSlot(slot))
		}
		rgbs[slot] = rgb
	}
	return branding.Palette{
		Primary:       rgbs["primary"],
		Secondary:     rgbs["secondary"],
		Accent:        rgbs["accent"],
		Foreground:    rgbs["foreground"],
		Background:    rgbs["background"],
		TextOnPrimary: rgbs["text_on_primary"],
		Source:        branding.PaletteSourceManual,
	}, nil
}

// parseHex decodes a "#rrggbb" string into an RGB triple. Uppercase
// is accepted; short-form ("#abc") is rejected for unambiguous round-
// tripping with branding.RGB.Hex.
func parseHex(in string) (branding.RGB, bool) {
	in = strings.ToLower(strings.TrimSpace(in))
	if !hexColorRE.MatchString(in) {
		return branding.RGB{}, false
	}
	r, err := strconv.ParseUint(in[1:3], 16, 8)
	if err != nil {
		return branding.RGB{}, false
	}
	g, err := strconv.ParseUint(in[3:5], 16, 8)
	if err != nil {
		return branding.RGB{}, false
	}
	b, err := strconv.ParseUint(in[5:7], 16, 8)
	if err != nil {
		return branding.RGB{}, false
	}
	return branding.RGB{R: uint8(r), G: uint8(g), B: uint8(b)}, true
}

// slotIsValid reports whether name is one of the six known palette
// slots. Used to gate POST /branding/palette/override.
func slotIsValid(name string) bool {
	for _, s := range paletteSlots {
		if s == name {
			return true
		}
	}
	return false
}

// humanSlot returns a user-facing label for the given slot name. It
// powers the validation error messages so the inline 422 fragment
// shows "primária" instead of "primary".
func humanSlot(slot string) string {
	switch slot {
	case "primary":
		return "primária"
	case "secondary":
		return "secundária"
	case "accent":
		return "destaque"
	case "foreground":
		return "texto"
	case "background":
		return "fundo"
	case "text_on_primary":
		return "texto sobre primária"
	}
	return slot
}

// validatePaletteContrast asserts the two AA-mandated pairs
// (Foreground/Background and TextOnPrimary/Primary) clear the
// branding.WCAGAANormalText threshold. The function returns a human
// message describing the failed pair when the palette fails; the
// caller surfaces it as the inline 422 fragment per AC #4.
func validatePaletteContrast(p branding.Palette) (string, bool) {
	if p.Foreground.Contrast(p.Background) < branding.WCAGAANormalText {
		return fmt.Sprintf(
			"Contraste insuficiente entre texto (%s) e fundo (%s): WCAG AA exige razão ≥ %.1f.",
			p.Foreground.Hex(), p.Background.Hex(), branding.WCAGAANormalText,
		), false
	}
	if p.TextOnPrimary.Contrast(p.Primary) < branding.WCAGAANormalText {
		return fmt.Sprintf(
			"Contraste insuficiente entre texto-sobre-primária (%s) e primária (%s): WCAG AA exige razão ≥ %.1f.",
			p.TextOnPrimary.Hex(), p.Primary.Hex(), branding.WCAGAANormalText,
		), false
	}
	return "", true
}
