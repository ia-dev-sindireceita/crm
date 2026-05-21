package usermfa

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// TenantAuditLogger satisfies mfa.AuditLogger by routing every event
// into the audit_log_security ledger via audit.SplitLogger.
//
// Each row carries tenant_id (captured at construction) and
// actor_user_id (the userID the mfa.Service supplied) so a master
// operator inspecting audit_log_security for a tenant can correlate
// MFA activity with the responsible user.
type TenantAuditLogger struct {
	writer   audit.SplitLogger
	tenantID uuid.UUID
}

var _ mfa.AuditLogger = (*TenantAuditLogger)(nil)

// NewTenantAuditLogger validates inputs and returns the adapter.
func NewTenantAuditLogger(writer audit.SplitLogger, tenantID uuid.UUID) (*TenantAuditLogger, error) {
	if writer == nil {
		return nil, fmt.Errorf("usermfa: NewTenantAuditLogger: writer is nil")
	}
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("usermfa: NewTenantAuditLogger: tenantID is uuid.Nil")
	}
	return &TenantAuditLogger{writer: writer, tenantID: tenantID}, nil
}

// LogEnrolled records the user's completion of a fresh enrolment.
func (l *TenantAuditLogger) LogEnrolled(ctx context.Context, userID uuid.UUID) error {
	return l.write(ctx, audit.SecurityEvent2FAEnroll, userID, nil)
}

// LogVerified records every successful TOTP verification.
func (l *TenantAuditLogger) LogVerified(ctx context.Context, userID uuid.UUID) error {
	return l.write(ctx, audit.SecurityEvent2FAVerify, userID, nil)
}

// LogRecoveryUsed records a single-use recovery code consumption. The
// row also flags reenroll_required upstream so the next session forces
// a fresh enrol.
func (l *TenantAuditLogger) LogRecoveryUsed(ctx context.Context, userID uuid.UUID) error {
	return l.write(ctx, audit.SecurityEvent2FARecoveryUsed, userID, nil)
}

// LogRecoveryRegenerated records a wholesale regen of the recovery
// code set.
func (l *TenantAuditLogger) LogRecoveryRegenerated(ctx context.Context, userID uuid.UUID) error {
	return l.write(ctx, audit.SecurityEvent2FARecoveryRegenerated, userID, nil)
}

// LogMFARequired records a bypass attempt: an authenticated principal
// reached a guarded endpoint without a TOTP-verified session. The
// reason field carries the upstream rationale (e.g. "missing_session",
// "alerter_failed:<cause>") and the route is the HTTP path that
// surfaced the deny so dashboards can split bypass attempts by area.
func (l *TenantAuditLogger) LogMFARequired(ctx context.Context, userID uuid.UUID, route, reason string) error {
	target := map[string]any{}
	if route != "" {
		target["route"] = route
	}
	if reason != "" {
		target["reason"] = reason
	}
	return l.write(ctx, audit.SecurityEvent2FARequired, userID, target)
}

func (l *TenantAuditLogger) write(ctx context.Context, evt audit.SecurityEvent, userID uuid.UUID, target map[string]any) error {
	tenantID := l.tenantID
	return l.writer.WriteSecurity(ctx, audit.SecurityAuditEvent{
		Event:       evt,
		ActorUserID: userID,
		TenantID:    &tenantID,
		Target:      target,
	})
}
