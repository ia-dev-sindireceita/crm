package usermfa

import (
	"context"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// NoopAlerter is the tenant counterpart of the master Slack alerter.
// Tenant MFA events do NOT page the master_ops channel — they live in
// audit_log_security only (the TenantAuditLogger row is the durable
// record). The mfa.Service requires a non-nil Alerter, so this no-op
// satisfies the port without side-effects.
//
// Should a future deploy want tenant-facing notifications (e.g. an
// admin's email when their recovery code is used), a real adapter
// replaces this one — the mfa.Service contract is unchanged.
type NoopAlerter struct{}

var _ mfa.Alerter = NoopAlerter{}

// AlertRecoveryUsed is a no-op.
func (NoopAlerter) AlertRecoveryUsed(_ context.Context, _ mfa.RecoveryUsedDetails) error {
	return nil
}

// AlertRecoveryRegenerated is a no-op.
func (NoopAlerter) AlertRecoveryRegenerated(_ context.Context, _ mfa.RecoveryRegeneratedDetails) error {
	return nil
}
