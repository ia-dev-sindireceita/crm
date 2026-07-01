package main

// SIN-66405 (from SIN-66378 P3 security review, SIN-66392 / PR #446):
// composition-root adapter that maps the web/channels access-change port
// (webchannels.AccessAuditor) onto the shared audit.SplitLogger, so
// grant / revoke / restricted-flip privilege events land in
// audit_log_security alongside the other privilege events (authz, master
// grant, wa_session transitions). Mirrors the masterMFAAuditLogger /
// masterSessionHardCapAuditor adapters in master_mfa_store_wire.go.

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
	webchannels "github.com/pericles-luz/crm/internal/web/channels"
)

// channelAccessAuditor adapts webchannels.AccessAuditor onto the split
// audit ledger. Writes are best-effort (OWASP A09 wants the trail, but the
// mutation has already committed): a failed write is warn-logged and never
// propagated, matching authz.AuditRecorder.Record.
type channelAccessAuditor struct {
	writer audit.SplitLogger
	log    *slog.Logger
	now    func() time.Time
}

// newChannelAccessAuditor builds the adapter. writer is required; a nil
// logger falls back to slog.Default. now is fixed to time.Now().UTC() and
// overridable in tests.
func newChannelAccessAuditor(writer audit.SplitLogger, log *slog.Logger) *channelAccessAuditor {
	if writer == nil {
		panic("channels_audit: writer is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	return &channelAccessAuditor{
		writer: writer,
		log:    log,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// ChannelAccessGranted writes one channel.access_granted row.
func (a *channelAccessAuditor) ChannelAccessGranted(ctx context.Context, actor, tenant, channelID, user uuid.UUID) {
	a.write(ctx, audit.SecurityEventChannelAccessGranted, actor, tenant, map[string]any{
		"channel_id": channelID.String(),
		"user_id":    user.String(),
	})
}

// ChannelAccessRevoked writes one channel.access_revoked row.
func (a *channelAccessAuditor) ChannelAccessRevoked(ctx context.Context, actor, tenant, channelID, user uuid.UUID) {
	a.write(ctx, audit.SecurityEventChannelAccessRevoked, actor, tenant, map[string]any{
		"channel_id": channelID.String(),
		"user_id":    user.String(),
	})
}

// ChannelRestrictedChanged writes one channel.restricted_changed row
// carrying the before/after flag.
func (a *channelAccessAuditor) ChannelRestrictedChanged(ctx context.Context, actor, tenant, channelID uuid.UUID, from, to bool) {
	a.write(ctx, audit.SecurityEventChannelRestrictedChanged, actor, tenant, map[string]any{
		"channel_id": channelID.String(),
		"from":       from,
		"to":         to,
	})
}

// write assembles + persists the SecurityAuditEvent. A nil tenant is
// impossible for these tenant-scoped events, but the SplitLogger tolerates
// it; the actor is always non-Nil (the web helper skips emission otherwise).
func (a *channelAccessAuditor) write(ctx context.Context, event audit.SecurityEvent, actor, tenant uuid.UUID, target map[string]any) {
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
		a.log.LogAttrs(ctx, slog.LevelWarn, "channel_access_audit_write_failed",
			slog.String("event", string(event)),
			slog.String("err", err.Error()),
		)
	}
}

// Compile-time guard: the adapter satisfies the web port.
var _ webchannels.AccessAuditor = (*channelAccessAuditor)(nil)
