package wallet

import (
	"time"

	"github.com/google/uuid"
)

// LedgerKind is the type of a token_ledger row's wallet-aware
// lifecycle. It mirrors the partial CHECK constraint added in
// migration 0089:
//
//	CHECK (wallet_id IS NULL OR kind IN ('reserve','commit','release','grant'))
//
// The string values are the canonical wire form persisted to the
// database; adapters must not rename them.
type LedgerKind string

const (
	// KindReserve records a Reserve operation. The amount column is
	// NEGATIVE (it represents an outflow from "available", though
	// balance itself is unchanged until Commit).
	KindReserve LedgerKind = "reserve"

	// KindCommit records the consummation of a reservation. The amount
	// column is NEGATIVE — the actual debit from balance.
	KindCommit LedgerKind = "commit"

	// KindRelease records a reservation rollback. The amount column is
	// POSITIVE — the reserved amount returns to available.
	KindRelease LedgerKind = "release"

	// KindGrant records a credit (courtesy onboarding, paid top-up).
	// The amount column is POSITIVE.
	KindGrant LedgerKind = "grant"
)

// LedgerEntry is one row of token_ledger in the wallet-aware lane
// (wallet_id IS NOT NULL). The append-only nature is enforced by
// REVOKE UPDATE on the table (migration 0089 only grants INSERT/SELECT
// to app_runtime; updates and deletes are master-ops with audit).
//
// IdempotencyKey is the load-bearing column: UNIQUE (wallet_id,
// idempotency_key) is the database-level guarantee that a retried
// operation collapses to "the prior row" rather than double-debiting.
//
// ExternalRef carries the upstream operation id (e.g. WhatsApp wamid,
// the original ReservationID for a Commit/Release follow-up). Both
// the reconciler and the operator UI use it to thread reserve →
// commit pairs together.
type LedgerEntry struct {
	ID             uuid.UUID
	WalletID       uuid.UUID
	TenantID       uuid.UUID
	Kind           LedgerKind
	Amount         int64
	IdempotencyKey string
	ExternalRef    string
	OccurredAt     time.Time
	CreatedAt      time.Time
}

// Reservation is the in-flight handle returned by Reserve. Use-case
// callers stash this value and pass it back to Commit or Release.
//
// The lifecycle is:
//
//	r := svc.Reserve(...)
//	defer ensureSettled(r)       // upstream watchdog / reconciler
//	resp, err := llm(...)
//	if err != nil { svc.Release(r, ...) }
//	svc.Commit(r, actualTokens, ...)
//
// The reservation's identity is the ledger row's IdempotencyKey on
// the original Reserve. Commit/Release rows link back via
// ExternalRef == Reservation.ID.String(). That single thread is what
// the F37 reconciler walks to find orphans.
type Reservation struct {
	ID             uuid.UUID
	WalletID       uuid.UUID
	TenantID       uuid.UUID
	Amount         int64
	IdempotencyKey string
	CreatedAt      time.Time
}

// SignedAmount returns the amount column as it must be written to
// token_ledger for this kind. Reserve/Commit are negative; Release/Grant
// are positive. Callers MUST go through this helper rather than
// hand-computing the sign at insert sites so the convention stays
// in one place.
func SignedAmount(kind LedgerKind, magnitude int64) int64 {
	switch kind {
	case KindReserve, KindCommit:
		return -magnitude
	case KindRelease, KindGrant:
		return magnitude
	default:
		// Unknown kind defaults to positive; the DB CHECK constraint
		// then rejects the row, surfacing the misuse loudly.
		return magnitude
	}
}
