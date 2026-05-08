// Package slog is the structured-logging adapter for the master MFA
// AuditLogger port (internal/iam/mfa). It writes one INFO-level
// record per audit event with a stable `event` attribute that ops
// dashboards and SIEM pipelines can grep / index.
//
// Hexagonal contract: this package depends on log/slog and the
// internal/iam/mfa port — nothing else. The Postgres-side
// master_ops_audit trail is written by the trigger from migration
// 0002 and is not duplicated here; this adapter exists so events that
// happen *before* a successful storage write (e.g. an MFA-required
// redirect) still land in the audit log.
package slog

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// Event names. Pinned by ADR 0074 §1, §2, §3, §5. Changing one of
// these breaks every dashboard that filters on the literal string.
const (
	EventEnrolled            = "master_mfa_enrolled"
	EventVerified            = "master_mfa_verified"
	EventRecoveryUsed        = "master_recovery_used"
	EventRecoveryRegenerated = "master_recovery_regenerated"
	EventMFARequired         = "master_mfa_required"
)

// MFAAudit implements mfa.AuditLogger by emitting one slog.Info record
// per call. The logger is supplied at construction so callers wire in
// whatever handler they prefer (text, json, otel, etc.).
type MFAAudit struct {
	logger *slog.Logger
}

// Compile-time assertion that the adapter satisfies the domain port.
var _ mfa.AuditLogger = (*MFAAudit)(nil)

// NewMFAAudit returns an MFAAudit. nil logger returns an error so a
// misconfigured deploy fails closed at bootstrap rather than silently
// dropping audit records — audit-with-no-output is worse than refusing
// to start.
func NewMFAAudit(logger *slog.Logger) (*MFAAudit, error) {
	if logger == nil {
		return nil, fmt.Errorf("slog: NewMFAAudit: logger is nil")
	}
	return &MFAAudit{logger: logger}, nil
}

// LogEnrolled records "master_mfa_enrolled".
func (a *MFAAudit) LogEnrolled(ctx context.Context, userID uuid.UUID) error {
	a.logger.InfoContext(ctx, EventEnrolled,
		slog.String("event", EventEnrolled),
		slog.String("user_id", userID.String()),
	)
	return nil
}

// LogVerified records "master_mfa_verified". Called by the verify
// handler on every successful TOTP submission (lands in PR4).
func (a *MFAAudit) LogVerified(ctx context.Context, userID uuid.UUID) error {
	a.logger.InfoContext(ctx, EventVerified,
		slog.String("event", EventVerified),
		slog.String("user_id", userID.String()),
	)
	return nil
}

// LogRecoveryUsed records "master_recovery_used". The Slack alerter
// fires alongside this on the same code path (ADR 0074 §5).
func (a *MFAAudit) LogRecoveryUsed(ctx context.Context, userID uuid.UUID) error {
	a.logger.InfoContext(ctx, EventRecoveryUsed,
		slog.String("event", EventRecoveryUsed),
		slog.String("user_id", userID.String()),
	)
	return nil
}

// LogRecoveryRegenerated records "master_recovery_regenerated".
func (a *MFAAudit) LogRecoveryRegenerated(ctx context.Context, userID uuid.UUID) error {
	a.logger.InfoContext(ctx, EventRecoveryRegenerated,
		slog.String("event", EventRecoveryRegenerated),
		slog.String("user_id", userID.String()),
	)
	return nil
}

// LogMFARequired records "master_mfa_required" — the deny-by-default
// signal from RequireMasterMFA (lands in PR5). reason is one of
// "not_enrolled" / "not_verified" so dashboards can split the two.
func (a *MFAAudit) LogMFARequired(ctx context.Context, userID uuid.UUID, route, reason string) error {
	a.logger.InfoContext(ctx, EventMFARequired,
		slog.String("event", EventMFARequired),
		slog.String("user_id", userID.String()),
		slog.String("route", route),
		slog.String("reason", reason),
	)
	return nil
}
