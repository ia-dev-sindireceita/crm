// Package noop implements email.EmailSender as a silent drop. It is
// the default wiring in dev environments (EMAIL_PROVIDER unset or
// "noop") so a developer can run the app end-to-end without
// configuring a real provider and without the recorder's bookkeeping.
//
// The adapter still runs Validate so call-site contract violations
// surface in dev exactly as they would in prod — only the network I/O
// is skipped.
package noop

import (
	"context"

	"github.com/pericles-luz/crm/internal/notify/email"
)

// Sender is a stateless email.EmailSender that discards every message.
type Sender struct{}

// New returns a noop Sender. The zero value is also valid.
func New() Sender { return Sender{} }

// Send validates msg and returns nil. Validation errors still surface
// (wrapping email.ErrInvalidMessage) so dev wiring catches malformed
// payloads before they reach prod.
func (s Sender) Send(_ context.Context, msg email.Message) error {
	return msg.Validate()
}

// Compile-time port assertion.
var _ email.EmailSender = Sender{}
