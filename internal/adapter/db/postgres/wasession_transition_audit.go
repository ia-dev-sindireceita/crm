package postgres

// SIN-66305 (R3 / SIN-66292, origin SIN-66260 Fase 5): tamper-evident audit
// of a WhatsApp-session terminal status transition (ban / disconnect).
//
// Why a dedicated adapter rather than calling SplitAuditLogger directly:
// the transition is observed asynchronously in the inbound pump goroutine,
// OUTSIDE any HTTP request, so app.tenant_id is not set on the connection.
// app_runtime is NOBYPASSRLS, so a bare INSERT into audit_log_security would
// be rejected by the tenant_isolation_insert policy (WITH CHECK tenant_id =
// current_setting('app.tenant_id')). We therefore run the write inside
// WithTenant(tenantID) — set_config pins the GUC to the session's tenant so
// the RLS WITH CHECK passes — and reuse SplitAuditLogger.WriteSecurity for
// the actual row (the pgx.Tx satisfies AuditExecutor), so the SecurityEvent
// vocabulary validation and the canonical INSERT shape are shared, not
// re-implemented.

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

// WASessionTransitionAuditor writes one audit_log_security row per
// WhatsApp-session terminal status transition.
type WASessionTransitionAuditor struct {
	pool    TxBeginner
	actorID uuid.UUID
	now     func() time.Time
}

// NewWASessionTransitionAuditor builds the auditor. actorID is the reserved
// system principal (iam.SystemPrincipalID) the async, operator-less
// transition is attributed to; a nil pool or zero actor fail eagerly so
// cmd/server fails at boot rather than on the first ban.
func NewWASessionTransitionAuditor(pool *pgxpool.Pool, actorID uuid.UUID) (*WASessionTransitionAuditor, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, ErrZeroActor
	}
	return &WASessionTransitionAuditor{
		pool:    pool,
		actorID: actorID,
		now:     func() time.Time { return time.Now().UTC() },
	}, nil
}

// RecordTransition persists one tamper-evident row for a session moving to
// `to` (from `from`, optional `reason`). evt MUST be one of
// audit.SecurityEventWASessionBanned / SecurityEventWASessionDisconnected;
// SplitAuditLogger rejects any other event via its controlled vocabulary.
//
// LGPD / PII-minimization (gate 5): the jsonb target carries only the
// lifecycle states and the transport tag — never the MSISDN/phone. The
// session is identified by its tenant (the audit row's tenant_id column);
// no phone is available on a StatusChange in the first place.
func (a *WASessionTransitionAuditor) RecordTransition(ctx context.Context, tenantID uuid.UUID, evt audit.SecurityEvent, from, to, reason string) error {
	if tenantID == uuid.Nil {
		return ErrZeroTenant
	}
	target := map[string]any{
		"transport": "wa_session",
		"from":      from,
		"to":        to,
	}
	if reason != "" {
		target["reason"] = reason
	}
	tid := tenantID
	occurred := a.now()
	return WithTenant(ctx, a.pool, tenantID, func(tx pgx.Tx) error {
		// pgx.Tx satisfies AuditExecutor; reuse the canonical writer so the
		// vocabulary guard + INSERT shape are shared with every other
		// security-audit caller.
		writer := &SplitAuditLogger{db: tx}
		return writer.WriteSecurity(ctx, audit.SecurityAuditEvent{
			Event:       evt,
			ActorUserID: a.actorID,
			TenantID:    &tid,
			Target:      target,
			OccurredAt:  occurred,
		})
	})
}
