package audit

// SIN-63958 / master-impersonation-spec §3.1: correlation_id context
// propagation. The ImpersonationFromSession middleware attaches the
// active master_impersonation_session.id to the request context so
// every downstream authz_allow / authz_deny / data-access row carries
// the link without each call site having to thread the id explicitly.
//
// The audit writer (postgres.SplitAuditLogger.WriteSecurity) consults
// CorrelationIDFromContext as a fallback when the event itself does
// not carry a CorrelationID. Pre-existing call sites that never
// populate the field continue to emit SQL NULL — unchanged behaviour.

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// correlationCtxKey is the unexported context-key type for the
// impersonation envelope id. Callers in other packages route through
// ContextWithCorrelationID / CorrelationIDFromContext rather than
// reach for the raw key, so the audit-correlation channel stays
// type-safe and grep-able.
type correlationCtxKey struct{}

// ContextWithCorrelationID returns a derived context that carries id
// as the active impersonation envelope correlation. The middleware
// calls this once per request after resolving the active row; the
// audit writer reads it back via CorrelationIDFromContext.
//
// A uuid.Nil argument is treated as "no correlation" — the context
// value is not set so a stale handler does not accidentally tag rows
// with a zero uuid.
func ContextWithCorrelationID(ctx context.Context, id uuid.UUID) context.Context {
	if id == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, correlationCtxKey{}, id)
}

// CorrelationIDFromContext returns the impersonation envelope id
// attached by the middleware, if any. The bool is false when no
// envelope is active for the request — the audit writer treats that
// as "emit SQL NULL", matching the column default.
func CorrelationIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(correlationCtxKey{}).(uuid.UUID)
	if !ok || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}

// SecurityRow is the shape returned by adapters that read back rows
// from audit_log_security — currently the Feed SSE handler that
// streams every event tagged with the active correlation_id. The
// struct mirrors the column set; Target is the decoded jsonb.
//
// Lives in the audit package (rather than impersonation) so any future
// reader of audit_log_security can reuse it without creating an
// impersonation → audit cycle.
type SecurityRow struct {
	ID            uuid.UUID
	TenantID      *uuid.UUID
	ActorUserID   uuid.UUID
	Event         SecurityEvent
	CorrelationID *uuid.UUID
	Target        map[string]any
	OccurredAt    time.Time
}
