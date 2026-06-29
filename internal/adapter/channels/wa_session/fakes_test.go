package wa_session

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// fakeInbound records the last InboundEvent it received and returns a
// configurable error, standing in for the receive-inbound use case so
// the adapter tests never touch a database.
type fakeInbound struct {
	calls int
	last  inbox.InboundEvent
	err   error
}

func (f *fakeInbound) HandleInbound(_ context.Context, ev inbox.InboundEvent) error {
	f.calls++
	f.last = ev
	return f.err
}

// fakeSender records the arguments of the last SendText call and returns
// a configurable (id, err), standing in for the whatsmeow session.
type fakeSender struct {
	calls    int
	tenantID uuid.UUID
	to       string
	body     string
	id       string
	err      error
}

func (f *fakeSender) SendText(_ context.Context, tenantID uuid.UUID, to, body string) (string, error) {
	f.calls++
	f.tenantID = tenantID
	f.to = to
	f.body = body
	return f.id, f.err
}

// fakeFlag is a deny/allow gate with a configurable error.
type fakeFlag struct {
	enabled bool
	err     error
}

func (f fakeFlag) Enabled(_ context.Context, _ uuid.UUID) (bool, error) {
	return f.enabled, f.err
}

// fakeRate returns a fixed allow/retryAfter/err.
type fakeRate struct {
	allow      bool
	retryAfter time.Duration
	err        error
	calls      int
	lastKey    string
}

func (f *fakeRate) Allow(_ context.Context, key string, _ time.Duration, _ int) (bool, time.Duration, error) {
	f.calls++
	f.lastKey = key
	return f.allow, f.retryAfter, f.err
}

// allowAllRate is the common "never throttles" limiter for tests that
// exercise paths other than rate limiting.
func allowAllRate() *fakeRate { return &fakeRate{allow: true} }

// enabledFlag / disabledFlag are the two common gate fixtures.
func enabledFlag() fakeFlag  { return fakeFlag{enabled: true} }
func disabledFlag() fakeFlag { return fakeFlag{enabled: false} }
