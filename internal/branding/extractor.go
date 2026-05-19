package branding

import (
	"context"
	"errors"
	"io"
)

// Hint carries advisory metadata about the source bytes. Adapters MAY
// use ContentType to short-circuit decoder selection, but MUST still
// validate the actual bytes (magic-byte sniff) — upload-time format
// validation is documented in ADR 0080 and is not duplicated here.
//
// MaxBytes is the byte budget the adapter is allowed to read from
// src. A value ≤ 0 means "use the adapter's default" (typically
// 2 MiB, matching ADR 0080's tenant-logo cap).
type Hint struct {
	ContentType string
	MaxBytes    int64
}

// PaletteExtractor is the domain port for deriving a Palette from a
// tenant logo.
//
// Implementations MUST:
//   - honour ctx (deadline, cancellation) on all I/O,
//   - read at most Hint.MaxBytes (or the default) bytes from src,
//   - return an error wrapping ErrUnsupportedFormat for inputs whose
//     decoded format the adapter does not handle (the upload layer
//     already rejects SVG and unknown types — this is defence in
//     depth),
//   - return an error wrapping ErrTooLarge for inputs that exceed the
//     byte or pixel budget,
//   - return an error wrapping ErrUnavailable for transient failures
//     (out-of-memory while decoding, dependency crash) — producers
//     fall back to the default neutral palette on this sentinel,
//   - return an error wrapping ErrInvalidImage for unrecoverable
//     decode failures (truncated bytes, declared format mismatch),
//   - return a Palette satisfying the WCAG AA contrast guarantees
//     described in ADR 0060 on success.
//
// Implementations MUST be deterministic: the same input bytes always
// yield the same Palette. This is required so the "revert to default
// palette" UX is well-defined and so unit tests can pin exact RGB
// triples.
type PaletteExtractor interface {
	Extract(ctx context.Context, src io.Reader, hint Hint) (Palette, error)
}

// Sentinels for caller-side classification. Wrap with
// fmt.Errorf("…: %w", branding.ErrFoo) so errors.Is keeps working
// through the chain.
var (
	// ErrUnsupportedFormat marks inputs whose format the adapter
	// cannot decode (e.g. SVG, TIFF). The upload layer (ADR 0080)
	// already filters formats; this sentinel exists for defence in
	// depth and for adapters with stricter format subsets.
	ErrUnsupportedFormat = errors.New("branding: unsupported logo format")
	// ErrTooLarge marks inputs that exceed Hint.MaxBytes or the
	// adapter's declared pixel ceiling. Producers SHOULD surface a
	// 413-equivalent UI message and NOT retry.
	ErrTooLarge = errors.New("branding: logo exceeds size budget")
	// ErrInvalidImage marks unrecoverable decode failures — truncated
	// bytes, content-type mismatch, malformed chunks. Treat as a
	// permanent error; do not retry.
	ErrInvalidImage = errors.New("branding: logo failed to decode")
	// ErrUnavailable marks transient failures inside the adapter
	// (memory pressure, dependency crash). Producers fall back to the
	// neutral default palette and record metric outcome=error.
	ErrUnavailable = errors.New("branding: palette extractor unavailable")
)
