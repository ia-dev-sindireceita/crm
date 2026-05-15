// Package scanner is the domain port for asynchronous media scanning.
//
// The scanning pipeline is hexagonal: the upload path persists a
// `message.media` row with `scan_status="pending"`; a worker
// ([SIN-62788] F2-05c) pulls keys off NATS and asks any MediaScanner
// adapter (e.g. ClamAV, [SIN-62788] F2-05b) to classify the object.
// The domain owns the contract — the closed set of Status values that
// may end up in `message.media`, the shape of ScanResult, and the
// interface every concrete adapter must satisfy.
//
// Invariants enforced by construction:
//
//   - No `database/sql`, no SDK, no transport. The port is plain Go
//     types plus context for cancellation. CI enforces this via the
//     `forbidimport` analyzer ([SIN-62216]).
//   - The Status enum is closed and string-typed so the JSON value
//     persisted into `message.media -> scan_status` is identical to
//     the Go constant — no parallel wire-vs-Go tables to drift.
//   - ScanResult carries EngineID so audit/forensics can correlate a
//     stored row against the exact engine+signature version that
//     produced the verdict.
package scanner

import "context"

// Status is the closed set of states persisted into
// `message.media -> scan_status`. Anything outside this set is a
// programmer or data-corruption bug — see Valid.
type Status string

// Known Status values. Strings are the wire/JSON representation and
// MUST match migration 0092's `message.media -> scan_status` comment.
const (
	// StatusPending is the initial value at upload time, before any
	// scanner verdict.
	StatusPending Status = "pending"

	// StatusClean means a scanner reported the object as safe to serve.
	StatusClean Status = "clean"

	// StatusInfected means a scanner reported the object as a threat;
	// the quarantine flow ([SIN-62788] F2-05d) moves the blob and
	// hides the message in the UI.
	StatusInfected Status = "infected"
)

// Valid reports whether s is one of the three closed Status values.
// Callers that read `scan_status` back from the `message.media` jsonb
// column should run it through Valid to guard against drift before
// branching on s.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusClean, StatusInfected:
		return true
	}
	return false
}

// ScanResult is the value a MediaScanner returns for a single object.
//
// Status is the verdict — StatusClean or StatusInfected. A scanner
// MUST NOT return StatusPending; that value belongs to the upload
// path and means "not scanned yet".
//
// EngineID is the implementation-specific identifier of the engine
// and signature set that produced the verdict (e.g. "clamav-1.4.2")
// and is persisted alongside the status so a future re-scan or audit
// reviewer can tell exactly which engine cleared or rejected the
// object. SHOULD be non-empty for production scanners; the in-process
// fake in tests may leave it empty.
type ScanResult struct {
	Status   Status
	EngineID string
}

// MediaScanner is the port the worker calls to classify a single
// stored object identified by its storage key (the same path produced
// by `internal/media/upload.StoragePath`, e.g.
// `media/<tenant>/<yyyy-mm>/<hash>.<ext>`).
//
// Implementations MUST:
//
//   - honour ctx for cancellation and deadlines, returning ctx.Err()
//     promptly when ctx is done;
//   - return a non-nil error on any I/O, transport, or engine-side
//     failure (the worker retries per its own policy);
//   - return (ScanResult, nil) only when an engine reached a verdict —
//     a scanner that cannot reach its backend MUST return an error,
//     not StatusClean.
//
// The port is intentionally narrow: no batching, no streaming, no
// configuration knobs. Adapters wrap vendor-specific shapes (TCP
// clamd INSTREAM, HTTP API, …) behind this single method.
type MediaScanner interface {
	Scan(ctx context.Context, key string) (ScanResult, error)
}
