package branding_test

import (
	"errors"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestErrPaletteNotFound_IsSentinel asserts the not-found marker is a
// distinct error value (not a string wrapper) so callers can use
// errors.Is to discriminate the negative-cache branch from a transient
// adapter failure.
func TestErrPaletteNotFound_IsSentinel(t *testing.T) {
	t.Parallel()
	if branding.ErrPaletteNotFound == nil {
		t.Fatal("ErrPaletteNotFound is nil")
	}
	wrapped := errors.Join(errors.New("wrapper"), branding.ErrPaletteNotFound)
	if !errors.Is(wrapped, branding.ErrPaletteNotFound) {
		t.Fatal("errors.Is failed to recognise wrapped sentinel")
	}
	if errors.Is(branding.ErrPaletteNotFound, branding.ErrUnavailable) {
		t.Fatal("sentinels collided — ErrPaletteNotFound matched ErrUnavailable")
	}
}
