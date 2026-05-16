// Package email defines the transactional email domain port and the
// canonical Message type used by every producer (IAM password reset,
// wallet alerts, billing/PIX notifications, LGPD notices, etc.).
//
// The package is deliberately storage- and transport-agnostic: it
// imports nothing beyond the Go standard library so any caller (a
// usecase, an HTTP handler, a worker) can wire it without dragging
// vendor SDKs along. Concrete delivery is the responsibility of an
// adapter under internal/adapter/notify/email/{mailgun,noop,recorder}.
//
// PII discipline (ADR 0004): callers MUST NOT log Message values
// directly — even Subject can leak PII (e.g. "Reset password for
// alice@example.com"). Adapters log only structural metadata
// (recipient count, byte size, message-id, provider response status).
package email

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Address is a single email participant. Name is the optional display
// name that providers render as e.g. `Acme Notifications
// <noreply@acme.com>`. Empty Name is rendered as the bare address.
type Address struct {
	Email string
	Name  string
}

// String renders the address in RFC 5322 short form. Used by adapters
// when the wire format requires a plain string (e.g. Mailgun's `from`
// field). The Name is NOT quoted — adapters that need stricter
// encoding (control characters, commas) MUST escape it themselves.
func (a Address) String() string {
	if a.Name == "" {
		return a.Email
	}
	return a.Name + " <" + a.Email + ">"
}

// Attachment is a single file attached to a Message. Content is read
// once by the adapter; callers wishing to send the same payload to
// multiple recipients should construct a new Reader (e.g.
// bytes.NewReader) per Send call.
type Attachment struct {
	Filename    string
	ContentType string
	Content     io.Reader
}

// Message is the canonical transactional email payload.
//
// Required fields: From.Email, at least one To, Subject, and at least
// one of Text or HTML. Validate enforces the contract so adapters can
// trust their input and avoid duplicating boundary checks.
//
// Headers is an open extension point for provider-neutral header
// passthrough (e.g. "List-Unsubscribe", "Auto-Submitted"). Reserved
// headers (From/To/Cc/Bcc/Reply-To/Subject/Content-Type) MUST NOT
// appear here — adapters set those from the structured fields and
// will reject duplicates.
type Message struct {
	From        Address
	To          []Address
	Cc          []Address
	Bcc         []Address
	ReplyTo     *Address
	Subject     string
	Text        string
	HTML        string
	Headers     map[string]string
	Attachments []Attachment
}

// reservedHeaders is the case-insensitive set of headers whose value
// is determined by the structured Message fields. Allowing callers to
// set these via Headers would silently override the structured value
// or — worse — let an attacker inject extra recipients via a CRLF in
// a free-form Bcc.
var reservedHeaders = map[string]struct{}{
	"from":         {},
	"to":           {},
	"cc":           {},
	"bcc":          {},
	"reply-to":     {},
	"subject":      {},
	"content-type": {},
}

// Validate returns an error wrapping ErrInvalidMessage if the message
// is unsendable. Adapters call this first so a malformed payload
// never reaches the network.
func (m Message) Validate() error {
	if m.From.Email == "" {
		return invalidf("from address is required")
	}
	if len(m.To) == 0 {
		return invalidf("at least one To recipient is required")
	}
	for i, a := range m.To {
		if a.Email == "" {
			return invalidf("To recipient %d has empty email", i)
		}
	}
	if m.Subject == "" {
		return invalidf("subject is required")
	}
	if m.Text == "" && m.HTML == "" {
		return invalidf("at least one of Text or HTML body is required")
	}
	if strings.ContainsAny(m.Subject, "\r\n") {
		return invalidf("subject contains forbidden CR/LF (header injection)")
	}
	for k, v := range m.Headers {
		if k == "" {
			return invalidf("header has empty key")
		}
		if _, reserved := reservedHeaders[strings.ToLower(k)]; reserved {
			return invalidf("header %q is reserved and must be set via the structured field", k)
		}
		if strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") {
			return invalidf("header %q contains forbidden CR/LF (header injection)", k)
		}
	}
	return nil
}

// EmailSender is the domain port for sending transactional email.
//
// Implementations MUST:
//   - honour ctx (deadline, cancellation) on all I/O,
//   - return an error wrapping ErrTransient for retryable failures
//     (network blip, 5xx, 429, context.DeadlineExceeded),
//   - return an error wrapping ErrPermanent for non-retryable failures
//     (4xx other than 408/429, auth failure, malformed domain),
//   - return an error wrapping ErrInvalidMessage for boundary errors
//     surfaced by Message.Validate.
//
// Producers therefore decide retry policy with a single errors.Is
// switch and never need to parse provider-specific error strings.
type EmailSender interface {
	Send(ctx context.Context, msg Message) error
}

// Sentinels for caller-side classification. Wrap with fmt.Errorf("…:
// %w", ErrTransient) so errors.Is keeps working through the chain.
var (
	// ErrTransient marks failures that may succeed on retry.
	ErrTransient = errors.New("email: transient send failure")
	// ErrPermanent marks failures that MUST NOT be retried.
	ErrPermanent = errors.New("email: permanent send failure")
	// ErrInvalidMessage marks boundary errors detected before any
	// network I/O. Caller code should treat it as a programmer bug.
	ErrInvalidMessage = errors.New("email: invalid message")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidMessage}, args...)...)
}
