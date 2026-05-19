// Package customdomain_verifier hosts the polling worker for
// SIN-63080 (Fase 5 / DNS-poller). It scans tenant_custom_domains for
// rows in the implicit pending_dns state — not deleted, not yet
// verified, not paused, not already failed — and invokes the
// customdomain/management.UseCase.Verify path on each one so the user
// does not have to click "Verificar agora" in the UI after the TXT
// record propagates.
//
// The package follows the hexagonal rule of the rest of the codebase
// (see internal/worker/dunning and internal/worker/funnel_engine): no
// database/sql, pgx, or HTTP imports. Three small ports cover the I/O
// the worker needs:
//
//   - Store        — paged listing of rows that should be polled, plus
//     the MarkFailed sink the worker uses to retire a
//     domain after exhausting the attempt cap.
//   - Verifier     — the customdomain/management Verify port the worker
//     invokes per row. In production this is the real
//     *management.UseCase; tests pass a fake.
//   - AuditLogger  — fire-and-forget structured-event sink so an
//     operator can answer "why did the worker stop
//     polling this row?" without joining the audit log.
//
// The package owns the orchestration: tick scheduling, per-domain
// exponential backoff, in-memory attempt cap, and Prometheus metrics.
// Adapters live in internal/adapter/store/postgres (Store) and the
// command-line wiring lives in cmd/customdomain-verifier.
package customdomain_verifier

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/customdomain/management"
)

// Store is the persistence port the verifier worker reads + writes
// through. The production implementation is
// internal/adapter/store/postgres.CustomDomainStore; tests use an
// in-memory fake.
//
// ListPendingVerification returns rows the worker should consider this
// tick, scoped at the SQL layer by the partial index
// idx_tenant_custom_domains_pending_verification. The worker further
// filters by its in-memory backoff before invoking Verify.
//
// MarkFailed flips failed_at + failure_reason. The worker calls it
// exactly once per domain that exhausts the attempt cap; the row then
// drops out of ListPendingVerification on the next tick.
type Store interface {
	ListPendingVerification(ctx context.Context) ([]management.Domain, error)
	MarkFailed(ctx context.Context, id uuid.UUID, at time.Time, reason string) (management.Domain, error)
}

// Verifier is the narrow projection of management.UseCase the worker
// needs. Returning the VerifyOutcome (which is what the management
// use-case already returns) keeps the audit-and-metric branches in this
// package; the management package's own audit log captures the per-
// attempt outcome.
type Verifier interface {
	Verify(ctx context.Context, tenantID, id uuid.UUID) (management.VerifyOutcome, error)
}

// AuditLogger records "give-up" events when the worker hits the attempt
// cap. nil-safe: the worker skips the call when the field is nil.
// Fire-and-forget — implementations must not block.
type AuditLogger interface {
	LogVerifierGiveUp(ctx context.Context, ev GiveUpEvent)
}

// GiveUpEvent is the flat structure the audit sink receives. Mirrors
// management.AuditEvent so the slog adapter can render it the same way.
type GiveUpEvent struct {
	TenantID uuid.UUID
	DomainID uuid.UUID
	Host     string
	Reason   string
	Attempts int
	At       time.Time
}

// Outcome classifies a single Verify attempt for backoff + metrics
// purposes. The worker derives one of these from the VerifyOutcome /
// error returned by Verifier.Verify.
type Outcome int

const (
	// OutcomeVerified — the domain transitioned pending_dns → verified.
	OutcomeVerified Outcome = iota + 1
	// OutcomeAlreadyVerified — the row was verified before this tick;
	// the worker should drop it from its in-memory bookkeeping.
	OutcomeAlreadyVerified
	// OutcomeMismatch — TXT record missing or wrong value. Transient by
	// default (the user has not propagated yet); counts toward the cap.
	OutcomeMismatch
	// OutcomeResolverError — DNS resolver returned an error. Transient.
	OutcomeResolverError
	// OutcomeBlockedSSRF — validator blocked the host (private IP /
	// loopback / blocklist hit). The worker does NOT count this toward
	// the cap — the user fixed the DNS to point at private IP space and
	// re-pointing it correctly should restart polling.
	OutcomeBlockedSSRF
	// OutcomeInternal — the Verify call itself errored (DB unreachable,
	// validator misconfigured, etc.). Counts toward the cap but logs
	// at error level so an operator notices.
	OutcomeInternal
)

// String maps Outcome to the metric label.
func (o Outcome) String() string {
	switch o {
	case OutcomeVerified:
		return "verified"
	case OutcomeAlreadyVerified:
		return "already_verified"
	case OutcomeMismatch:
		return "mismatch"
	case OutcomeResolverError:
		return "resolver_error"
	case OutcomeBlockedSSRF:
		return "blocked_ssrf"
	case OutcomeInternal:
		return "internal"
	default:
		return "unknown"
	}
}

// Failure reasons persisted in tenant_custom_domains.failure_reason.
const (
	// FailureReasonCapExceeded is the default cap-exhaustion reason.
	FailureReasonCapExceeded = "cap_exceeded"
)
