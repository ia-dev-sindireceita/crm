// MessageMediaStore is the persistence port the worker uses to record
// a scan verdict against an existing `message.media` row. Defined in
// the domain package (alongside MediaScanner) so the worker depends on
// closed value types — no SQL, no JSON shape — and concrete adapters
// (e.g. internal/adapter/db/postgres/messagemedia) wire to it.
//
// Idempotency contract: implementations MUST refuse to overwrite a
// terminal verdict. A row whose stored scan_status is already
// StatusClean or StatusInfected is finalised and a second call with a
// new ScanResult is a no-op that returns ErrAlreadyFinalised. The
// worker uses that sentinel to ack the redelivered NATS message
// without performing duplicate work (AC: "se scan_status já é
// clean/infected, no-op").
//
// Missing-row contract: a request for a (tenantID, messageID) pair
// that does not exist (RLS-hidden cross-tenant, or genuinely deleted)
// returns ErrNotFound. The worker treats this as a poison message and
// acks it — re-delivering would never succeed.

package scanner

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrAlreadyFinalised is returned by MessageMediaStore.UpdateScanResult
// when the target row's scan_status is already StatusClean or
// StatusInfected. Callers test with errors.Is.
var ErrAlreadyFinalised = errors.New("scanner: message.media scan_status already finalised")

// ErrNotFound is returned when no message row matches the
// (tenantID, messageID) pair under the tenant RLS scope.
var ErrNotFound = errors.New("scanner: message not found")

// MessageMediaStore persists the verdict produced by MediaScanner.Scan.
// One method, by design: the worker has exactly one persistence call
// per delivered NATS message. The port stays in the domain package so
// dependent code never reaches for `database/sql` or pgx.
type MessageMediaStore interface {
	// UpdateScanResult patches the row's `media` jsonb column with
	// the verdict. Returns ErrAlreadyFinalised on a redelivery against
	// an already-terminal row, ErrNotFound when the row is missing,
	// or a wrapped underlying error on transport failures (which the
	// worker treats as retryable and does NOT ack).
	UpdateScanResult(ctx context.Context, tenantID, messageID uuid.UUID, result ScanResult) error
}
