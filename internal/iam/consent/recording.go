package consent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

// RecordingRegistry decorates a ConsentRegistry with audit_log_data
// emission. Each Record that actually creates a row writes one
// DataEventConsentGrant; each Revoke writes one DataEventConsentRevoke.
// Idempotent Record no-ops do NOT re-emit the audit row — re-emitting
// would create spurious LGPD trail entries that suggest the operator
// acted twice when they did not.
//
// The decorator depends on audit.SplitLogger so it shares the same
// audit pool wiring as every other LGPD-relevant primitive
// (impersonation, lgpd_export, etc.). cmd/server passes the
// production SplitAuditLogger backed by the app_audit pool.
//
// ActorUserID for the audit event is resolved from ctx via
// ActorFromContext at call time; missing actor returns
// audit.ErrSplitAuditEventInvalid (wrapped by the SplitLogger), the
// decorator does NOT swallow that error — the underlying consent
// write is rolled forward but the audit failure bubbles up so the
// caller knows the trail is incomplete. This matches the
// aipolicy.RecordingRepository contract.
type RecordingRegistry struct {
	inner ConsentRegistry
	audit audit.SplitLogger
	now   func() time.Time
	actor func(context.Context) (uuid.UUID, bool)
}

// RecordingConfig parameterises NewRecordingRegistry. ActorFromContext
// is required so the decorator can pull the request-scope actor out
// of the call site without importing the HTTP layer. Now defaults to
// time.Now in UTC.
type RecordingConfig struct {
	Now              func() time.Time
	ActorFromContext func(context.Context) (uuid.UUID, bool)
}

// NewRecordingRegistry wraps inner so every successful Record/Revoke
// emits one audit_log_data event. nil inner, audit, or
// ActorFromContext yields a typed error; cmd/server fails fast at
// boot rather than swallowing a misconfigured wiring.
func NewRecordingRegistry(inner ConsentRegistry, auditLogger audit.SplitLogger, cfg RecordingConfig) (*RecordingRegistry, error) {
	if inner == nil {
		return nil, ErrNilStore
	}
	if auditLogger == nil {
		return nil, ErrNilAuditLogger
	}
	if cfg.ActorFromContext == nil {
		return nil, fmt.Errorf("consent: NewRecordingRegistry: ActorFromContext is required")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &RecordingRegistry{
		inner: inner,
		audit: auditLogger,
		now:   now,
		actor: cfg.ActorFromContext,
	}, nil
}

// compile-time assertion: RecordingRegistry implements the port.
var _ ConsentRegistry = (*RecordingRegistry)(nil)

// Record delegates to the inner registry and, when a new row is
// created, emits one DataEventConsentGrant. Idempotent no-ops skip
// the audit emission (the operator did not produce a new LGPD event).
func (r *RecordingRegistry) Record(ctx context.Context, rec ConsentRecord) (ConsentRecord, bool, error) {
	if err := validateRecord(rec); err != nil {
		return ConsentRecord{}, false, err
	}
	actor, ok := r.actor(ctx)
	if !ok || actor == uuid.Nil {
		return ConsentRecord{}, false, fmt.Errorf("consent: Record: missing actor")
	}
	persisted, created, err := r.inner.Record(ctx, rec)
	if err != nil {
		return ConsentRecord{}, false, err
	}
	if !created {
		return persisted, false, nil
	}
	if err := r.audit.WriteData(ctx, audit.DataAuditEvent{
		Event:       audit.DataEventConsentGrant,
		ActorUserID: actor,
		TenantID:    persisted.TenantID,
		Target:      auditTarget(persisted, ""),
		OccurredAt:  r.now(),
	}); err != nil {
		return persisted, true, fmt.Errorf("consent: Record: audit: %w", err)
	}
	return persisted, true, nil
}

// Latest delegates to the inner registry. Reads are not audited.
func (r *RecordingRegistry) Latest(ctx context.Context, tenant uuid.UUID, subject Subject, purpose Purpose) (*ConsentRecord, error) {
	return r.inner.Latest(ctx, tenant, subject, purpose)
}

// History delegates to the inner registry. Reads are not audited.
func (r *RecordingRegistry) History(ctx context.Context, tenant uuid.UUID, subject Subject, purpose Purpose) ([]ConsentRecord, error) {
	return r.inner.History(ctx, tenant, subject, purpose)
}

// Revoke delegates to the inner registry and emits one
// DataEventConsentRevoke on success. ErrNoActiveGrant is NOT audited
// — there is nothing to revoke and therefore nothing to attribute.
func (r *RecordingRegistry) Revoke(ctx context.Context, q RevokeQuery) (ConsentRecord, error) {
	if err := validateRevoke(q); err != nil {
		return ConsentRecord{}, err
	}
	actor, ok := r.actor(ctx)
	if !ok || actor == uuid.Nil {
		return ConsentRecord{}, fmt.Errorf("consent: Revoke: missing actor")
	}
	persisted, err := r.inner.Revoke(ctx, q)
	if err != nil {
		return ConsentRecord{}, err
	}
	if err := r.audit.WriteData(ctx, audit.DataAuditEvent{
		Event:       audit.DataEventConsentRevoke,
		ActorUserID: actor,
		TenantID:    persisted.TenantID,
		Target:      auditTarget(persisted, q.Reason),
		OccurredAt:  r.now(),
	}); err != nil {
		return persisted, fmt.Errorf("consent: Revoke: audit: %w", err)
	}
	return persisted, nil
}

// auditTarget renders the per-event jsonb payload. The shape is
// stable so a master-panel reader can deserialize without trusting
// per-purpose schema variation: every consent_grant / consent_revoke
// row carries the same keys.
func auditTarget(rec ConsentRecord, reason string) map[string]any {
	target := map[string]any{
		"consent_id":   rec.ID.String(),
		"subject_type": string(rec.Subject.Type),
		"subject_id":   rec.Subject.ID,
		"purpose":      string(rec.Purpose),
		"version":      rec.Version,
	}
	if rec.IP.IsValid() {
		target["ip"] = rec.IP.String()
	}
	if rec.UserAgent != "" {
		target["user_agent"] = rec.UserAgent
	}
	if reason != "" {
		target["reason"] = reason
	}
	return target
}

// validateRecord rejects ConsentRecord values that violate the
// domain invariants before reaching the adapter. The adapter applies
// the same checks against the underlying CHECK constraints; the
// domain rejects earlier so callers see a typed sentinel.
func validateRecord(rec ConsentRecord) error {
	if rec.TenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	if !rec.Subject.Type.IsValid() {
		return ErrInvalidSubjectType
	}
	if strings.TrimSpace(rec.Subject.ID) == "" {
		return ErrInvalidSubjectID
	}
	if !rec.Purpose.IsValid() {
		return ErrInvalidPurpose
	}
	if strings.TrimSpace(rec.Version) == "" {
		return ErrInvalidVersion
	}
	return nil
}

// validateRevoke rejects RevokeQuery values that violate the domain
// invariants before reaching the adapter.
func validateRevoke(q RevokeQuery) error {
	if q.TenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	if !q.Subject.Type.IsValid() {
		return ErrInvalidSubjectType
	}
	if strings.TrimSpace(q.Subject.ID) == "" {
		return ErrInvalidSubjectID
	}
	if !q.Purpose.IsValid() {
		return ErrInvalidPurpose
	}
	if strings.TrimSpace(q.Reason) == "" {
		return ErrInvalidRevokeReason
	}
	return nil
}
