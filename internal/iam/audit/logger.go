// Package audit holds the per-tenant business-event audit log port.
// SIN-62219 introduces it for the master impersonation flow; future
// PRs will use the same Logger for role grants, exports, and other
// non-repudiation-critical events.
//
// Domain code only depends on this package. The postgres-backed writer
// and the impersonation middleware live in adapters and import this
// port — never the other way around.
package audit

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Event names emitted by the master impersonation middleware. They are
// declared here (not in the middleware) so other adapters and tests can
// grep / assert on the canonical strings without taking a dependency on
// the http layer.
const (
	EventImpersonationStarted = "impersonation_started"
	EventImpersonationEnded   = "impersonation_ended"
)

// AuditEvent is the value the Logger writes to audit_log. Fields map
// 1:1 onto the table columns (migration 0007_create_audit_log):
//
//	tenant_id      = TenantID         (NOT NULL — pointer kept for
//	                                   future cross-tenant events that
//	                                   may carry NULL; today's two
//	                                   impersonation events always
//	                                   set it).
//	actor_user_id  = ActorUserID      (the master user id for the
//	                                   impersonation events).
//	event          = Event            (one of EventImpersonation*).
//	target         = Target           (jsonb; reason / duration_ms
//	                                   live here).
//	created_at     = CreatedAt        (zero time → DEFAULT now()).
//
// AuditEvent is intentionally a plain struct: zero-value-friendly so
// tests can omit fields they don't care about, and free of any
// database/sql or pgx imports so the iam layer stays adapter-free.
type AuditEvent struct {
	Event       string
	ActorUserID uuid.UUID
	TenantID    *uuid.UUID
	Target      map[string]any
	CreatedAt   time.Time
}

// Logger is the port the impersonation middleware (and future
// audited operations) call. Implementations MUST persist the event
// synchronously and return a non-nil error if the write did not commit
// — non-repudiation requires that the caller can refuse to proceed
// when the trail cannot be written.
//
// Returning a wrapped error preserves errors.Is behaviour for callers
// that want to disambiguate transient infra failures from a hard fail.
type Logger interface {
	Log(ctx context.Context, event AuditEvent) error
}
