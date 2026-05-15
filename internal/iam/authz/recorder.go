package authz

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
)

// AuditRecorder is the production Recorder: it persists Decisions into
// audit_log_security via the SplitLogger and increments the Prometheus
// counters on Metrics. Construction validates required dependencies.
type AuditRecorder struct {
	writer  audit.SplitLogger
	metrics *Metrics
	log     *slog.Logger
}

// NewAuditRecorder builds a recorder. writer and metrics are required;
// log may be nil and falls back to slog.Default.
//
// The recorder is safe to share across goroutines — both the writer
// and the Prometheus instruments are designed for concurrent use.
func NewAuditRecorder(writer audit.SplitLogger, metrics *Metrics, log *slog.Logger) *AuditRecorder {
	if writer == nil {
		panic("authz: AuditRecorder writer is nil")
	}
	if metrics == nil {
		panic("authz: AuditRecorder metrics is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	return &AuditRecorder{writer: writer, metrics: metrics, log: log}
}

// Record writes one audit_log_security row for d and increments the
// metric counters. Audit-write failures are warn-logged but do NOT
// surface back to the wrapper — the Decision has already been returned
// to the caller and observability is best-effort by design.
//
// When p.UserID is uuid.Nil the audit write is skipped entirely (the
// audit_log_security.actor_user_id FK would reject it); the deny is
// still counted in the dashboard metric so an unauthenticated probing
// burst remains visible.
func (r *AuditRecorder) Record(ctx context.Context, p iam.Principal, action iam.Action, res iam.Resource, d iam.Decision, now time.Time) {
	r.metrics.Observe(p, action, d)
	if p.UserID == uuid.Nil {
		r.log.LogAttrs(ctx, slog.LevelWarn, "authz_audit_skipped_no_actor",
			slog.String("action", string(action)),
			slog.String("reason_code", string(d.ReasonCode)),
			slog.Bool("allow", d.Allow),
		)
		return
	}

	outcome := "deny"
	event := audit.SecurityEventAuthzDeny
	if d.Allow {
		outcome = "allow"
		event = audit.SecurityEventAuthzAllow
	}

	var tenantID *uuid.UUID
	if p.TenantID != uuid.Nil {
		t := p.TenantID
		tenantID = &t
	}

	if err := r.writer.WriteSecurity(ctx, audit.SecurityAuditEvent{
		Event:       event,
		ActorUserID: p.UserID,
		TenantID:    tenantID,
		Target: map[string]any{
			"outcome":     outcome,
			"action":      string(action),
			"reason_code": string(d.ReasonCode),
			"target_kind": d.TargetKind,
			"target_id":   d.TargetID,
		},
		OccurredAt: now,
	}); err != nil {
		// Best-effort: log but do not propagate. The Decision is
		// already committed at the HTTP layer and probing attempts
		// remain visible via authz_user_deny_total.
		r.log.LogAttrs(ctx, slog.LevelWarn, "authz_audit_write_failed",
			slog.String("action", string(action)),
			slog.String("reason_code", string(d.ReasonCode)),
			slog.String("outcome", outcome),
			slog.String("err", err.Error()),
		)
	}
}
