// Package quarantine is the domain port the mediascan-worker calls when
// a scan returns scanner.StatusInfected. Implementations move the
// infected blob from the runtime media bucket (`media/<tenant>/…`) into
// an isolated `media-quarantine` bucket that the application IAM cannot
// read or list, so the rest of the system cannot accidentally serve the
// payload back to a user. See [SIN-62805] F2-05d.
//
// The port is intentionally narrow:
//
//   - One method: Move(ctx, key). Concrete adapters perform a single
//     server-side copy then delete, so the worker depends on closed
//     value types — no S3 SDK, no transport.
//   - Idempotent. Implementations MUST treat a Move against an
//     already-moved (or already-deleted) key as a no-op: redeliveries
//     of `media.scan.requested` against an already-finalised row hit
//     scanner.ErrAlreadyFinalised in the store and never reach Move,
//     but a crash between Move and the persistence ack can re-execute
//     this path. Returning success in that case is safe.
//   - Best-effort against transient network errors: callers (the
//     worker) treat a non-nil error as a redeliverable failure and let
//     the NATS broker schedule another attempt.
//
// The port lives next to the other media domain ports
// (scanner.MediaScanner, scanner.MessageMediaStore) so the worker keeps
// its hexagonal boundary clean — no `database/sql`, no S3 SDK in
// `internal/media/worker`.
package quarantine

import "context"

// Quarantiner is the single method the worker calls on an infected
// verdict. Implementations are expected to be safe for concurrent use:
// the worker fans out infected verdicts across its semaphore-bounded
// goroutine pool.
type Quarantiner interface {
	// Move relocates the object at key from the runtime media bucket
	// into the quarantine bucket. The key passed here is the same
	// storage path the worker received on the `media.scan.requested`
	// payload (i.e. the value produced by
	// `internal/media/upload.StoragePath`, e.g.
	// `<tenant>/<yyyy-mm>/<hash>.<ext>` — note: no leading `media/`
	// prefix because that prefix is the *bucket*, not part of the key).
	//
	// Implementations MUST honour ctx for cancellation and SHOULD
	// surface I/O errors with enough detail for log triage. A nil
	// return means the object is now in the quarantine bucket and no
	// longer reachable from the runtime IAM.
	Move(ctx context.Context, key string) error
}
