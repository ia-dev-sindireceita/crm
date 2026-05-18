package pix

// Status is the PIX-charge lifecycle status. Values match migration
// 0102's pix_charges.status CHECK constraint exactly — adding a new
// status requires a new migration AND a new constant here AND a new
// entry in IsKnown.
//
// Deviation from the SIN-62956 issue text: the spec lists
// {pending, paid, expired, failed} but migration 0102 (already merged
// in SIN-62952) uses {pending, paid, expired, cancelled} where
// `cancelled` is the admin-driven "abandon this charge" terminal
// (which also covers permanent PSP failures — the receiver translates
// PSP-error webhooks to Cancel at the use-case layer). The migration
// is authoritative; this package matches it.
type Status string

const (
	// StatusPending is the initial status: the charge has been created
	// in our database (and ideally registered with the PSP), but no
	// terminal event has fired yet. This is the only status from which
	// MarkPaid / Expire / Cancel may transition.
	StatusPending Status = "pending"

	// StatusPaid is the happy-path terminal: a `paid` webhook from the
	// PSP for this external_id was processed. paid_at MUST be set when
	// status is paid (mirrors the DB CHECK
	// pix_charges_paid_at_consistency).
	StatusPaid Status = "paid"

	// StatusExpired is the TTL terminal: expires_at elapsed without a
	// matching `paid` webhook. The PSP guarantees the BR Code is
	// unredeemable past expires_at; we mirror that in our domain so
	// the UI can stop offering the QR.
	StatusExpired Status = "expired"

	// StatusCancelled is the abandon terminal: an admin or use case
	// explicitly cancelled the charge (e.g. invoice voided, PSP
	// returned a non-retryable error). Terminal; no further transitions.
	StatusCancelled Status = "cancelled"
)

// IsTerminal reports whether the status forbids further transitions.
// pending is the only non-terminal status.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusPaid, StatusExpired, StatusCancelled:
		return true
	default:
		return false
	}
}

// IsKnown reports whether s is one of the four canonical values. Used
// by adapters at hydration time to defensively reject corrupted rows.
func (s Status) IsKnown() bool {
	switch s {
	case StatusPending, StatusPaid, StatusExpired, StatusCancelled:
		return true
	default:
		return false
	}
}
