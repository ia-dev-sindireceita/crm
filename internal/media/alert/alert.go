// Package alert is the domain port the mediascan-worker calls when a
// scan returns scanner.StatusInfected. The worker emits a single,
// auditable record per infection so the security on-call gets paged in
// real-time without scraping logs. The transport (Slack today, PagerDuty
// tomorrow) lives behind the Alerter interface so the worker keeps its
// hexagonal boundary clean — no HTTP client, no JSON wire shape leaks
// into `internal/media/worker`. See [SIN-62805] F2-05d.
package alert

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Event is the closed shape every Alerter receives. Field names match
// the [SIN-62805] AC ("{tenant_id, message_id, engine_id, signature}").
// Signature is the human-readable verdict line emitted by the engine
// (e.g. "Win.Test.EICAR_HDB-1"); when the engine does not provide a
// signature, callers pass the empty string and adapters render a
// generic "unknown signature" placeholder so the wire never carries an
// empty field that could be mistaken for "no infection".
type Event struct {
	TenantID  uuid.UUID
	MessageID uuid.UUID
	Key       string
	EngineID  string
	Signature string
}

// Alerter is the single method the worker calls. Production wiring
// targets the Slack #security webhook; unit tests inject a recording
// fake. Implementations MUST honour ctx for cancellation and SHOULD
// avoid blocking the worker for longer than a few seconds — the worker
// treats a non-nil Notify error as a redeliverable failure (the
// upstream NATS broker will redeliver, and the redelivery hits
// scanner.ErrAlreadyFinalised in the store, then re-invokes Notify
// idempotently).
type Alerter interface {
	Notify(ctx context.Context, event Event) error
}

// Noop is the default Alerter used at boot when no alert transport is
// configured. It does nothing, returns nil, and exists so the worker
// can be wired with a non-nil collaborator without a guard at every
// call site.
type Noop struct{}

// Notify implements Alerter; returns nil unconditionally.
func (Noop) Notify(_ context.Context, _ Event) error { return nil }

// ErrEmptyEvent is returned by adapters that reject incomplete Event
// values (zero tenant + message ids). Callers test with errors.Is.
var ErrEmptyEvent = errors.New("alert: event missing tenant_id or message_id")
