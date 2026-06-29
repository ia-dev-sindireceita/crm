package wasession

import (
	"context"

	"github.com/google/uuid"
)

// Sink is how a Device reports events (inbound messages, status changes, QR
// codes) back to the Manager. The Manager implements it; Devices never hold
// a reference to the Manager directly, only to this narrow port.
type Sink interface {
	Emit(Event)
}

// Device is one tenant's live WhatsApp Web session transport. The concrete
// implementation (whatsmeowdev.device) wraps a *whatsmeow.Client; the domain
// only sees this port.
//
// Connect blocks: it establishes the session and returns when the session is
// torn down (ctx cancelled) or it cannot be (re)established. The Manager's
// supervisor calls Connect in a loop, applying reconnect backoff between
// returns, so an implementation may either block for the lifetime of the
// connection or return on each disconnect — both are supervised correctly.
type Device interface {
	// Connect establishes the session. When the device is not yet paired it
	// initiates QR pairing, emitting EventQR via the Sink until paired or ctx
	// is cancelled. It returns when the connection ends.
	Connect(ctx context.Context) error
	// Disconnect tears down the live connection without clearing credentials.
	Disconnect()
	// SendText sends a plain-text message to a recipient phone in E.164 (no
	// '+') and returns the carrier-assigned message id.
	SendText(ctx context.Context, toE164, body string) (externalID string, err error)
	// Paired reports whether persisted credentials exist for this tenant.
	Paired() bool
}

// DeviceFactory builds a Device for a tenant, wiring its events into sink.
// The concrete factory owns the shared Postgres credential store (ADR 0107
// D3) and hands each Device the per-tenant slice of it.
type DeviceFactory interface {
	NewDevice(ctx context.Context, tenantID uuid.UUID, sink Sink) (Device, error)
}
