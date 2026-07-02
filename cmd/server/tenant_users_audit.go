package main

// SIN-66499: composition-root adapter that maps the tenantusers.Auditor port
// onto the shared audit.SplitLogger, so user create / role-change /
// deactivate privilege events land in audit_log_security alongside the other
// privilege events. Mirrors channelAccessAuditor in channels_audit.go.

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

// tenantUserAuditor adapts tenantusers.Auditor onto the split audit ledger.
// Writes are best-effort (OWASP A09 wants the trail, but the mutation has
// already committed): a failed write is warn-logged and never propagated.
type tenantUserAuditor struct {
	writer audit.SplitLogger
	log    *slog.Logger
	now    func() time.Time
}

// newTenantUserAuditor builds the adapter. writer is required; a nil logger
// falls back to slog.Default. now is fixed to time.Now().UTC().
func newTenantUserAuditor(writer audit.SplitLogger, log *slog.Logger) *tenantUserAuditor {
	if writer == nil {
		panic("tenant_users_audit: writer is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	return &tenantUserAuditor{
		writer: writer,
		log:    log,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// UserCreated writes one tenant.user.created row.
func (a *tenantUserAuditor) UserCreated(ctx context.Context, actor, tenant, targetUser uuid.UUID, email string, role iam.Role) {
	a.write(ctx, audit.SecurityEventTenantUserCreated, actor, tenant, map[string]any{
		"user_id": targetUser.String(),
		"email":   email,
		"role":    string(role),
	})
}

// UserRoleChanged writes one tenant.user.role_changed row carrying the
// before/after role.
func (a *tenantUserAuditor) UserRoleChanged(ctx context.Context, actor, tenant, targetUser uuid.UUID, from, to iam.Role) {
	a.write(ctx, audit.SecurityEventTenantUserRoleChanged, actor, tenant, map[string]any{
		"user_id": targetUser.String(),
		"from":    string(from),
		"to":      string(to),
	})
}

// UserDeactivated writes one tenant.user.deactivated row.
func (a *tenantUserAuditor) UserDeactivated(ctx context.Context, actor, tenant, targetUser uuid.UUID, role iam.Role) {
	a.write(ctx, audit.SecurityEventTenantUserDeactivated, actor, tenant, map[string]any{
		"user_id": targetUser.String(),
		"role":    string(role),
	})
}

func (a *tenantUserAuditor) write(ctx context.Context, event audit.SecurityEvent, actor, tenant uuid.UUID, target map[string]any) {
	var tenantID *uuid.UUID
	if tenant != uuid.Nil {
		t := tenant
		tenantID = &t
	}
	if err := a.writer.WriteSecurity(ctx, audit.SecurityAuditEvent{
		Event:       event,
		ActorUserID: actor,
		TenantID:    tenantID,
		Target:      target,
		OccurredAt:  a.now(),
	}); err != nil {
		a.log.LogAttrs(ctx, slog.LevelWarn, "tenant_user_audit_write_failed",
			slog.String("event", string(event)),
			slog.String("err", err.Error()),
		)
	}
}

// Compile-time guard: the adapter satisfies the domain port.
var _ tenantusers.Auditor = (*tenantUserAuditor)(nil)
