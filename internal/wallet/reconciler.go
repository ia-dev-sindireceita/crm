package wallet

import (
	"time"

	"github.com/google/uuid"
)

// DefaultMaxReservationAge is the conservative default the F37
// reconciler uses to decide that an in-flight reservation is orphaned
// (the LLM call that was supposed to settle it never returned). Five
// minutes is loose enough to clear the longest legitimate LLM call we
// expect on Fase 1, tight enough that money does not sit "reserved"
// for half a day.
//
// The background-job adapter that will live in PR11 reads this default
// but accepts an override from config; the pure function below also
// accepts the max age as an argument so unit tests can dial it down.
const DefaultMaxReservationAge = 5 * time.Minute

// OrphanReservation is the reconciler's recommendation that a given
// in-flight reservation should be released. The job runner takes this
// value, picks an idempotency key tagged with the reservation id so
// retries collapse safely, and calls the Release use-case.
//
// The reservation may be safely re-checked under lock by the use-case
// — between this read-only sweep and the lock, an in-flight commit
// could have landed. The use-case's LookupCompletedByExternalRef path
// handles that by returning ErrReservationCompleted; the reconciler
// is therefore safe to act on stale data.
type OrphanReservation struct {
	WalletID       uuid.UUID
	TenantID       uuid.UUID
	ReservationID  uuid.UUID
	Amount         int64
	IdempotencyKey string
	ReservedAt     time.Time
}

// FindOrphanReservations is the pure core of the F37 nightly
// reconciliation. Given a list of "still-open" reserve ledger entries
// (the adapter populates this with a single SQL query — see
// Repository.ListOpenReservations), the current time, and the cut-off
// age, it returns the entries that should be released.
//
// The function is intentionally allocation-free apart from the result
// slice so it remains fast on wallets with thousands of historical
// rows. The job runner calls it once per wallet and acts on the
// return value.
//
// "Open" here means: a reserve row with no follow-up commit/release
// matching its external_ref. The caller (the adapter) is responsible
// for the join; this function never asks the database anything.
//
// Edge cases this function handles by construction:
//
//   - Empty input → empty output.
//   - Future ReservedAt (clock skew) → never released (Sub returns
//     negative, below maxAge).
//   - maxAge == 0 → every entry is released (still respects "open").
//   - Non-reserve kinds in the input → ignored. The reconciler treats
//     them as adapter bugs and skips them instead of mis-releasing.
func FindOrphanReservations(open []LedgerEntry, now time.Time, maxAge time.Duration) []OrphanReservation {
	if len(open) == 0 {
		return nil
	}
	out := make([]OrphanReservation, 0, len(open))
	for _, e := range open {
		if e.Kind != KindReserve {
			continue
		}
		if now.Sub(e.OccurredAt) < maxAge {
			continue
		}
		// The reservation id is the original Reserve's ExternalRef.
		// The use-case stamps it at Reserve time; absence here means
		// the adapter produced a malformed row.
		rid, err := uuid.Parse(e.ExternalRef)
		if err != nil {
			continue
		}
		out = append(out, OrphanReservation{
			WalletID:       e.WalletID,
			TenantID:       e.TenantID,
			ReservationID:  rid,
			Amount:         -e.Amount, // reserves are stored negative; the magnitude is what we release
			IdempotencyKey: e.IdempotencyKey,
			ReservedAt:     e.OccurredAt,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
