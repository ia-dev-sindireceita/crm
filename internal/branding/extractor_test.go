package branding

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrors_AreSentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{"unsupported", ErrUnsupportedFormat},
		{"too large", ErrTooLarge},
		{"invalid image", ErrInvalidImage},
		{"unavailable", ErrUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wrapped := fmt.Errorf("adapter: %w", tc.err)
			if !errors.Is(wrapped, tc.err) {
				t.Fatalf("errors.Is failed through wrap for %v", tc.err)
			}
			if wrapped.Error() == tc.err.Error() {
				t.Fatalf("wrapped error did not add context: %q", wrapped)
			}
		})
	}
}

func TestErrors_AreDistinct(t *testing.T) {
	t.Parallel()

	all := []error{ErrUnsupportedFormat, ErrTooLarge, ErrInvalidImage, ErrUnavailable}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Fatalf("sentinel %d aliases sentinel %d", i, j)
			}
		}
	}
}
