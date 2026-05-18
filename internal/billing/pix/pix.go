// Package pix is the PIX-charge domain for SIN-62956 / Fase 4
// ([SIN-62197](/SIN/issues/SIN-62197) AC#4 and AC#5).
//
// The domain owns three concepts:
//
//   - Status        — the enum {pending, paid, expired, cancelled} that
//     mirrors migration 0102's pix_charges.status CHECK constraint.
//   - PIXCharge     — the aggregate root: one row per (invoice,
//     external_id). external_id is assigned after the PSP responds and
//     is UNIQUE thereafter (partial UNIQUE index, see migration 0102).
//   - WebhookEvent  — the value type the PSP-specific receiver
//     (delivered in C13) translates from HTTP payloads before calling
//     the Reconciler port.
//
// Transitions are driven by three external signals:
//
//   - PSP webhook "paid"      → MarkPaid (pending → paid). Idempotent
//     by (external_id, event_type) at both the domain layer (a second
//     MarkPaid on a paid charge is a no-op) and the storage layer
//     (webhook_events UNIQUE constraint — see migration 0102).
//   - TTL elapsed             → Expire   (pending → expired). The cron
//     worker calls this once expires_at has passed.
//   - Admin / PSP permanent error → Cancel (pending → cancelled).
//
// Hexagonal / Ports & Adapters (AC#3):
//
//   - PIXCharger     — outbound port: create a charge against the PSP
//     and query its status. The Banco Inter adapter lands in C7
//     ([SIN-62958](/SIN/issues/SIN-62958)); this domain never imports
//     a PSP SDK.
//   - Reconciler     — inbound port: consume a normalised WebhookEvent
//     and apply the corresponding transition. The HTTP webhook
//     receiver (C13) wraps a Reconciler.
//   - Repository     — persistence port: upsert / fetch PIXCharge rows.
//     Postgres adapter lands alongside C7.
//   - EventLog       — idempotency-ledger port over the global
//     webhook_events table. Adapters MUST translate the UNIQUE
//     violation on (source, external_id, event_type) to
//     ErrDuplicateEvent.
//
// Domain code MUST stay free of database/sql, pgx, net/http and any PSP
// SDK imports. Persistence lives behind Repository; the Postgres
// adapter is the only blessed implementation.
//
// Reference: board decision D2 in
// [SIN-62205](/SIN/issues/SIN-62205) — Banco Inter ratified as the
// concrete PSP behind PIXCharger.
package pix

import (
	"time"

	"github.com/google/uuid"
)

// PIXCharge is the aggregate root for a PIX charge. One row per
// (invoice_id, external_id) — external_id starts empty during the
// brief pre-PSP-ack window and becomes UNIQUE once set.
//
// Invariants enforced by the constructors and transitions:
//
//   - tenantID and invoiceID are non-nil uuids.
//   - qrCode and copyPaste are non-empty (NOT NULL in migration 0102).
//   - expiresAt is strictly after createdAt.
//   - status is one of the four canonical values (see Status).
//   - paid_at is set iff status == paid (mirrors the DB CHECK
//     pix_charges_paid_at_consistency).
//   - externalID is either empty (pending pre-ack) or non-empty and
//     immutable thereafter; the partial UNIQUE index on
//     pix_charges.external_id enforces the database half.
//
// All accessors return value types; the entity owns mutation.
type PIXCharge struct {
	id         uuid.UUID
	tenantID   uuid.UUID
	invoiceID  uuid.UUID
	externalID string
	qrCode     string
	copyPaste  string
	status     Status
	paidAt     *time.Time
	expiresAt  time.Time
	createdAt  time.Time
	updatedAt  time.Time
}

// NewCharge constructs a fresh pending PIX charge. external_id is left
// empty — call AttachExternalID once the PSP returns its charge id.
//
// expiresAt MUST be strictly after now; qrCode and copyPaste MUST be
// non-empty. Returns the appropriate Err* sentinel on violation.
func NewCharge(
	tenantID, invoiceID uuid.UUID,
	qrCode, copyPaste string,
	expiresAt, now time.Time,
) (*PIXCharge, error) {
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	if invoiceID == uuid.Nil {
		return nil, ErrZeroInvoice
	}
	if qrCode == "" {
		return nil, ErrEmptyQRCode
	}
	if copyPaste == "" {
		return nil, ErrEmptyCopyPaste
	}
	if !expiresAt.After(now) {
		return nil, ErrExpiresAtInPast
	}
	return &PIXCharge{
		id:        uuid.New(),
		tenantID:  tenantID,
		invoiceID: invoiceID,
		qrCode:    qrCode,
		copyPaste: copyPaste,
		status:    StatusPending,
		expiresAt: expiresAt,
		createdAt: now,
		updatedAt: now,
	}, nil
}

// HydrateCharge rebuilds a PIXCharge from durable storage. Only
// adapters should call this; it bypasses the invariants enforced by
// NewCharge because the database has already vetted them.
//
// externalID is "" for rows that have not yet been registered with the
// PSP. paidAt is nil unless status == paid.
func HydrateCharge(
	id, tenantID, invoiceID uuid.UUID,
	externalID, qrCode, copyPaste string,
	status Status,
	paidAt *time.Time,
	expiresAt, createdAt, updatedAt time.Time,
) *PIXCharge {
	return &PIXCharge{
		id:         id,
		tenantID:   tenantID,
		invoiceID:  invoiceID,
		externalID: externalID,
		qrCode:     qrCode,
		copyPaste:  copyPaste,
		status:     status,
		paidAt:     paidAt,
		expiresAt:  expiresAt,
		createdAt:  createdAt,
		updatedAt:  updatedAt,
	}
}

// ID returns the charge's primary key.
func (c *PIXCharge) ID() uuid.UUID { return c.id }

// TenantID returns the owning tenant. Denormalised onto the row so RLS
// can enforce tenant isolation without joining through invoice.
func (c *PIXCharge) TenantID() uuid.UUID { return c.tenantID }

// InvoiceID returns the invoice this charge belongs to.
func (c *PIXCharge) InvoiceID() uuid.UUID { return c.invoiceID }

// ExternalID returns the PSP's charge identifier, or "" if the charge
// has not yet been registered with the PSP.
func (c *PIXCharge) ExternalID() string { return c.externalID }

// QRCode returns the base64-encoded PNG/SVG of the BR Code (served by
// the UI as a data URL). NOT NULL by construction.
func (c *PIXCharge) QRCode() string { return c.qrCode }

// CopyPaste returns the EMVCo "PIX copia-e-cola" string. NOT NULL by
// construction.
func (c *PIXCharge) CopyPaste() string { return c.copyPaste }

// Status returns the current status.
func (c *PIXCharge) Status() Status { return c.status }

// PaidAt returns the timestamp at which the `paid` webhook was
// applied, or nil if the charge has not been paid.
func (c *PIXCharge) PaidAt() *time.Time { return c.paidAt }

// ExpiresAt returns the TTL deadline. After this point Expire may be
// called to transition the charge to expired.
func (c *PIXCharge) ExpiresAt() time.Time { return c.expiresAt }

// CreatedAt returns the row's creation timestamp.
func (c *PIXCharge) CreatedAt() time.Time { return c.createdAt }

// UpdatedAt returns the row's last-mutation timestamp.
func (c *PIXCharge) UpdatedAt() time.Time { return c.updatedAt }

// IsTerminal reports whether the charge is in a terminal status
// (paid, expired, or cancelled). Convenience for callers that branch
// on liveness.
func (c *PIXCharge) IsTerminal() bool { return c.status.IsTerminal() }

// AttachExternalID sets the PSP-issued external_id on a fresh charge.
// Returns ErrEmptyExternalID for "", ErrExternalIDAlreadySet if
// external_id is already populated, and ErrInvalidTransition if the
// charge is no longer pending (assigning an external_id to a terminal
// charge would be meaningless and almost certainly a caller bug).
func (c *PIXCharge) AttachExternalID(externalID string, now time.Time) error {
	if externalID == "" {
		return ErrEmptyExternalID
	}
	if c.externalID != "" {
		return ErrExternalIDAlreadySet
	}
	if c.status != StatusPending {
		return ErrInvalidTransition
	}
	c.externalID = externalID
	c.updatedAt = now
	return nil
}

// MarkPaid applies a `paid` webhook to the charge.
//
// Transitions:
//
//   - pending  → paid (sets paid_at = now, returns changed=true).
//   - paid     → paid (idempotent no-op, returns changed=false, nil).
//     This is the AC #1 invariant: a duplicate webhook for the same
//     (external_id, event_type=paid) does NOT transition twice.
//   - expired  → ErrInvalidTransition (a paid event arriving after we
//     already declared the charge dead is a reconciliation bug; the
//     receiver should re-open via Cancel-then-create rather than
//     silently mutate the row).
//   - cancelled → ErrInvalidTransition (terminal).
//
// now is the timestamp recorded as paid_at on a fresh transition; on a
// duplicate no-op the existing paid_at is preserved.
func (c *PIXCharge) MarkPaid(now time.Time) (changed bool, err error) {
	switch c.status {
	case StatusPending:
		c.status = StatusPaid
		paid := now
		c.paidAt = &paid
		c.updatedAt = now
		return true, nil
	case StatusPaid:
		return false, nil
	default:
		return false, ErrInvalidTransition
	}
}

// Expire applies the TTL terminal to a pending charge.
//
// Transitions:
//
//   - pending where now > expires_at → expired (changed=true, nil).
//   - pending where now ≤ expires_at → ErrTTLNotElapsed.
//   - expired                        → no-op (idempotent, returns
//     changed=false, nil).
//   - paid                           → no-op (paid wins; an expiry
//     tick that races a payment must not undo the payment).
//   - cancelled                      → no-op.
//
// Idempotent on every terminal status so the cron worker can re-call
// Expire without checking the current status first.
func (c *PIXCharge) Expire(now time.Time) (changed bool, err error) {
	switch c.status {
	case StatusPending:
		if !now.After(c.expiresAt) {
			return false, ErrTTLNotElapsed
		}
		c.status = StatusExpired
		c.updatedAt = now
		return true, nil
	case StatusExpired, StatusPaid, StatusCancelled:
		return false, nil
	default:
		return false, ErrInvalidTransition
	}
}

// Cancel applies the admin / PSP-error terminal to a pending charge.
//
// Transitions:
//
//   - pending   → cancelled (changed=true, nil).
//   - cancelled → no-op (idempotent).
//   - paid      → ErrInvalidTransition (paid is the happy path; a
//     cancellation that arrives after payment must be handled in the
//     invoice / refund domain, not here).
//   - expired   → ErrInvalidTransition (expired charges are already
//     unredeemable; cancelling them adds nothing).
func (c *PIXCharge) Cancel(now time.Time) (changed bool, err error) {
	switch c.status {
	case StatusPending:
		c.status = StatusCancelled
		c.updatedAt = now
		return true, nil
	case StatusCancelled:
		return false, nil
	default:
		return false, ErrInvalidTransition
	}
}
