package pix

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ChargeRequest is the input to PIXCharger.Create. It carries the
// minimum the PSP needs to issue a charge plus our internal correlation
// ids; PSP-specific fields (e.g. Banco Inter's payerCpfCnpj layout)
// stay in the adapter.
type ChargeRequest struct {
	// TenantID owns the charge for RLS and audit.
	TenantID uuid.UUID

	// InvoiceID is the invoice this charge covers.
	InvoiceID uuid.UUID

	// AmountCents is the gross amount, in centavos. The PSP renders
	// "R$ 12,34" from 1234.
	AmountCents int64

	// PayerName is the human-readable name shown on the PSP receipt.
	// Required by Banco Inter; we forward it as-is.
	PayerName string

	// PayerDocument is the PIX payer's CPF (11 digits) or CNPJ (14
	// digits) as a string of digits only. Validation happens at the
	// caller (the invoice domain) — this port carries the wire format.
	PayerDocument string

	// ExpiresAt is the TTL we want the PSP to honour. Adapters MUST
	// round-trip this to the PSP's per-charge expiry field; the
	// internal pix_charges.expires_at is set from the same value.
	ExpiresAt time.Time
}

// ChargeResponse is the output of PIXCharger.Create after the PSP
// acknowledges the charge.
type ChargeResponse struct {
	// ExternalID is the PSP's charge identifier (Banco Inter calls
	// this `txid`). Non-empty by contract; adapters MUST translate
	// PSP errors to non-nil err rather than returning an empty
	// ExternalID.
	ExternalID string

	// QRCode is the base64-encoded image of the BR Code. Adapters
	// either pass through the PSP's rendering or render the EMVCo
	// payload locally — the domain does not care.
	QRCode string

	// CopyPaste is the EMVCo "PIX copia-e-cola" string.
	CopyPaste string
}

// PIXCharger is the outbound port for the PSP integration. The Banco
// Inter adapter lands in C7 ([SIN-62958](/SIN/issues/SIN-62958)); this
// domain never imports a PSP SDK (AC#3).
//
// Implementations MUST:
//
//   - return (ChargeResponse{}, err) on PSP failure, never an empty
//     ChargeResponse with nil err.
//   - keep no per-request mutable state — callers may invoke Create /
//     Status concurrently.
//   - honour context cancellation; long-running HTTP calls to the PSP
//     must respect ctx.Deadline.
type PIXCharger interface {
	// Create asks the PSP to issue a charge and returns its
	// identifiers. The caller persists a PIXCharge with the returned
	// ExternalID via AttachExternalID.
	Create(ctx context.Context, req ChargeRequest) (ChargeResponse, error)

	// Status queries the PSP for the live status of a charge. The
	// receiver (C13) uses this as a fallback when the webhook signal
	// is missing or to reconcile after an outage. Returns
	// ErrNotFound when the PSP claims the externalID is unknown.
	Status(ctx context.Context, externalID string) (Status, error)
}

// WebhookEventType enumerates the canonical webhook event types the
// reconciler understands. The HTTP webhook receiver (C13) normalises
// PSP-specific vocabulary (e.g. Banco Inter's "PAGAMENTO_RECEBIDO")
// to one of these values before calling Reconciler.Apply.
type WebhookEventType string

const (
	// WebhookEventPaid is the "charge was paid" notification.
	// Triggers MarkPaid.
	WebhookEventPaid WebhookEventType = "paid"

	// WebhookEventExpired is the "charge timed out" notification.
	// Most PSPs do not emit this — the cron worker that walks
	// pix_charges_status_idx is the primary driver of Expire — but
	// when the PSP does emit it (e.g. push reconciliation after
	// outage) the receiver translates to this event type.
	WebhookEventExpired WebhookEventType = "expired"

	// WebhookEventCancelled is the "charge cancelled by PSP" notification
	// (typically a permanent non-retryable failure). Triggers Cancel.
	WebhookEventCancelled WebhookEventType = "cancelled"
)

// IsKnown reports whether t is one of the canonical webhook event
// types. The reconciler rejects unknown event types with
// ErrUnknownEventType — the receiver is the place to translate
// PSP-specific vocabulary, not the domain.
func (t WebhookEventType) IsKnown() bool {
	switch t {
	case WebhookEventPaid, WebhookEventExpired, WebhookEventCancelled:
		return true
	default:
		return false
	}
}

// WebhookEvent is the normalised inbound message from the PSP. The
// HTTP webhook receiver (C13) is responsible for verifying the PSP's
// signature and translating the wire payload into this value before
// calling Reconciler.Apply.
type WebhookEvent struct {
	// Source is the PSP identifier (e.g. "banco-inter"). Mirrored to
	// webhook_events.source — part of the dedup key.
	Source string

	// ExternalID is the PSP's charge identifier. Looked up against
	// pix_charges.external_id (UNIQUE).
	ExternalID string

	// EventType is the normalised event-type. The reconciler rejects
	// unknown values with ErrUnknownEventType.
	EventType WebhookEventType

	// Payload is the raw PSP payload, retained for audit. Stored as
	//-is in webhook_events.payload (jsonb). Never inspected by the
	// domain.
	Payload []byte

	// OccurredAt is the PSP-claimed event timestamp. When absent the
	// receiver sets this to the receive time; the domain treats it
	// as the canonical "when did this happen" for the transition.
	OccurredAt time.Time
}

// Outcome describes what Reconciler.Apply did. It is returned even on
// the dedup happy-path so callers can render observability without
// re-querying the repository.
type Outcome struct {
	// Duplicate is true when EventLog.Record reported the event was
	// already present in the dedup ledger. Charge is nil and
	// Transitioned is false in this case.
	Duplicate bool

	// Transitioned is true when the state machine actually advanced
	// the charge (e.g. pending → paid). false for duplicate webhooks
	// AND for idempotent no-ops on the destination status (e.g. a
	// second `paid` event after dedup somehow misses).
	Transitioned bool

	// Charge is the charge after the transition, or nil on duplicate.
	// Callers (the receiver) typically log the new status for audit.
	Charge *PIXCharge
}

// Reconciler is the inbound port the HTTP webhook receiver (C13) calls
// after verifying the PSP signature. Apply is the only entry point;
// implementations orchestrate EventLog.Record (dedup) and
// Repository.Save (state) inside a single transactional boundary.
//
// Apply MUST be idempotent at the (Source, ExternalID, EventType)
// granularity: a second call with the same triple returns
// Outcome{Duplicate: true} without mutating the charge.
type Reconciler interface {
	Apply(ctx context.Context, evt WebhookEvent) (Outcome, error)
}

// Repository is the persistence port for PIXCharge. Mirrors the
// pix_charges table shape from migration 0102.
//
// Reads are tenant-scoped (app_runtime role with RLS). Writes require
// the master_ops role and record actorID in the master_ops audit
// trail (pix_charges_master_ops_audit trigger fires the audit row).
//
// Implementations MUST translate:
//
//   - "no rows"                                       → ErrNotFound
//   - UNIQUE violation on pix_charges_external_id_uniq → ErrExternalIDAlreadySet
//   - CHECK violation on pix_charges_paid_at_consistency → ErrInvalidTransition
//     (defence in depth — the domain catches it first).
type Repository interface {
	// GetByID returns the charge with the given primary key, or
	// ErrNotFound. Tenant-scoped via RLS.
	GetByID(ctx context.Context, id uuid.UUID) (*PIXCharge, error)

	// GetByExternalID returns the charge matching the PSP's
	// external_id, or ErrNotFound. external_id is partial-UNIQUE in
	// migration 0102 so this is a deterministic lookup. Used by the
	// reconciler when applying webhook events.
	GetByExternalID(ctx context.Context, externalID string) (*PIXCharge, error)

	// Save inserts or updates the charge. actorID is recorded in the
	// master_ops audit trail. Implementations MUST run inside
	// WithMasterOps so the audit trigger fires.
	Save(ctx context.Context, c *PIXCharge, actorID uuid.UUID) error

	// ListExpiredPending returns up to limit pending charges whose
	// expires_at has elapsed. Used by the cron worker that drives
	// Expire. Ordered by expires_at ASC so the oldest stuck charges
	// are processed first. Adapters MUST filter via the
	// pix_charges_status_idx partial index (WHERE status='pending')
	// to keep the scan bounded.
	ListExpiredPending(ctx context.Context, before time.Time, limit int) ([]*PIXCharge, error)
}

// EventLog is the idempotency-ledger port over the global
// webhook_events table (migration 0102). Adapters MUST insert with
// UNIQUE (source, external_id, event_type); on conflict, the adapter
// returns ErrDuplicateEvent.
//
// The ledger is intentionally NOT tenant-scoped — webhooks arrive
// before we know which tenant they affect — and lives outside RLS
// (migration 0102 explicitly disables RLS on webhook_events). It is
// keyed by (source, externalID, eventType) so the same charge can
// receive a `paid` AND a later `expired` push reconciliation without
// the second event being mistaken for a duplicate of the first.
type EventLog interface {
	// Record inserts the dedup row. Returns ErrDuplicateEvent if the
	// (source, externalID, eventType) tuple is already present. On
	// any other error the caller MUST NOT apply the transition —
	// retrying the webhook is safer than risking a missed dedup.
	Record(
		ctx context.Context,
		source, externalID string,
		eventType WebhookEventType,
		payload []byte,
		receivedAt time.Time,
	) error
}
