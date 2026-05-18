package pix

import "errors"

var (
	// ErrZeroTenant is returned when uuid.Nil is passed as a tenant id.
	ErrZeroTenant = errors.New("pix: tenant id must not be uuid.Nil")

	// ErrZeroInvoice is returned when uuid.Nil is passed as an invoice id.
	ErrZeroInvoice = errors.New("pix: invoice id must not be uuid.Nil")

	// ErrEmptyQRCode is returned by NewCharge when qrCode is empty.
	// qr_code is NOT NULL in migration 0102 so we reject this in the
	// constructor rather than letting the adapter raise a constraint
	// violation.
	ErrEmptyQRCode = errors.New("pix: qr_code must not be empty")

	// ErrEmptyCopyPaste is returned by NewCharge when copyPaste is empty.
	// copy_paste is NOT NULL in migration 0102.
	ErrEmptyCopyPaste = errors.New("pix: copy_paste must not be empty")

	// ErrExpiresAtInPast is returned by NewCharge when expiresAt is not
	// strictly after the supplied now. PIX TTLs are forward-only — a
	// charge created already-expired is a programming error.
	ErrExpiresAtInPast = errors.New("pix: expires_at must be after now")

	// ErrInvalidTransition is returned by state-machine methods
	// (MarkPaid, Expire, Cancel, AttachExternalID) when the receiver is
	// in an incompatible status. Idempotent no-ops on the destination
	// status do NOT return this error — they return (changed=false, nil).
	ErrInvalidTransition = errors.New("pix: invalid state transition")

	// ErrTTLNotElapsed is returned by Expire when called on a pending
	// charge whose expires_at is still in the future. Callers (cron,
	// reconciler) should only call Expire after observing now > expires_at.
	ErrTTLNotElapsed = errors.New("pix: expires_at has not elapsed yet")

	// ErrExternalIDAlreadySet is returned by AttachExternalID when the
	// receiver already has a non-empty external_id. external_id is
	// immutable once assigned — adapters MUST translate the partial
	// UNIQUE index pix_charges_external_id_uniq violation to this
	// sentinel as well (defence in depth).
	ErrExternalIDAlreadySet = errors.New("pix: external_id is already set")

	// ErrEmptyExternalID is returned by AttachExternalID when
	// externalID is the empty string. The PSP never returns an empty
	// charge identifier; receiving "" is always a bug in the caller.
	ErrEmptyExternalID = errors.New("pix: external_id must not be empty")

	// ErrNotFound is returned by Repository methods when no row exists
	// for the requested key. Adapters MUST translate "no rows" to this
	// sentinel so callers can match with errors.Is without importing
	// pgx.
	ErrNotFound = errors.New("pix: not found")

	// ErrDuplicateEvent is returned by EventLog.Record when the
	// (source, external_id, event_type) tuple is already present in
	// the webhook idempotency ledger. Adapters MUST translate the
	// UNIQUE violation on webhook_events_dedup_uniq to this sentinel;
	// the reconciler treats it as the dedup signal and returns
	// Outcome{Duplicate: true}, nil to the caller.
	ErrDuplicateEvent = errors.New("pix: duplicate webhook event")

	// ErrUnknownEventType is returned by the reconciler when the
	// webhook event_type is not one of the canonical values
	// ("paid", "expired", "cancelled"). The receiver normalises the
	// PSP-specific vocabulary before calling Apply.
	ErrUnknownEventType = errors.New("pix: unknown event_type")
)
