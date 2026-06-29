package wasession

import (
	"fmt"
	"io"
	"log/slog"
)

// redacted is the placeholder rendered in place of any secret value.
const redacted = "[REDACTED]"

// Credential wraps secret session material — currently the QR pairing code,
// which is itself a bearer secret: anyone who can read it can hijack the
// pairing. Its Stringer, GoStringer, fmt formatter, JSON marshaler and
// slog.LogValuer all render a redaction placeholder so the value can never
// leak into logs, error strings or serialized payloads by accident (ADR
// 0107 D6). Code that genuinely needs the value (the QR-rendering UI in a
// later phase) must call Reveal explicitly.
type Credential struct {
	value string
}

// NewCredential wraps a secret value.
func NewCredential(value string) Credential { return Credential{value: value} }

// Reveal returns the underlying secret. This is the only path that exposes
// it; every other rendering of Credential is redacted.
func (c Credential) Reveal() string { return c.value }

// IsZero reports whether the credential holds no value.
func (c Credential) IsZero() bool { return c.value == "" }

// String implements fmt.Stringer with a redacted value.
func (c Credential) String() string { return redacted }

// GoString implements fmt.GoStringer so %#v also redacts.
func (c Credential) GoString() string { return redacted }

// Format implements fmt.Formatter so every verb (%v, %s, %q, %#v, ...)
// redacts. Without this, %q on the Stringer path would still quote the
// placeholder but a struct-printing verb could otherwise expose the field.
func (c Credential) Format(f fmt.State, verb rune) {
	switch verb {
	case 'q':
		fmt.Fprintf(f, "%q", redacted)
	default:
		io.WriteString(f, redacted)
	}
}

// MarshalJSON renders the redaction placeholder so the secret never ends up
// in a serialized event or audit record.
func (c Credential) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redacted + `"`), nil
}

// LogValue implements slog.LogValuer so structured logging redacts too.
func (c Credential) LogValue() slog.Value { return slog.StringValue(redacted) }
