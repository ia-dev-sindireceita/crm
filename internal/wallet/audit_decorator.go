package wallet

// SIN-62883 / Fase 2.5 C8: audit decorator over MasterGrantRepository.
//
// The decorator wraps any MasterGrantRepository and emits one
// audit_log_security row per Create call. Outcome=allow on success,
// outcome=deny on persistence error — both rows commit synchronously so
// the audit trail captures the master operator's intent regardless of
// whether the grant landed.
//
// Wiring layer (cmd/server) is expected to wrap the postgres
// MasterGrantStore with this decorator before handing it to use cases.
// Reads (GetByID, ListByTenant) and Revoke are not audited by this
// decorator — the existing master_ops_audit trigger already captures
// them at the DB layer.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

// AuditedMasterGrantRepository decorates a MasterGrantRepository with
// a synchronous audit hook on Create.
type AuditedMasterGrantRepository struct {
	inner  MasterGrantRepository
	writer audit.SplitLogger
	now    func() time.Time
	log    *slog.Logger
}

// NewAuditedMasterGrantRepository wires the decorator. inner and writer
// are required; now defaults to time.Now and log defaults to
// slog.Default. The decorator's audit hook is best-effort by design:
// a writer failure is warn-logged but never surfaces back to the
// caller because the grant has already been persisted (the use case
// must remain idempotent).
func NewAuditedMasterGrantRepository(inner MasterGrantRepository, writer audit.SplitLogger, now func() time.Time, log *slog.Logger) (*AuditedMasterGrantRepository, error) {
	if inner == nil {
		return nil, fmt.Errorf("wallet: AuditedMasterGrantRepository: inner repository is nil")
	}
	if writer == nil {
		return nil, fmt.Errorf("wallet: AuditedMasterGrantRepository: audit writer is nil")
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &AuditedMasterGrantRepository{
		inner:  inner,
		writer: writer,
		now:    now,
		log:    log,
	}, nil
}

// Create persists the grant and emits one master.grant.issued audit
// row. Outcome=allow on success; outcome=deny when inner.Create
// returns an error. The inner error is returned unchanged so the
// caller can match with errors.Is.
func (r *AuditedMasterGrantRepository) Create(ctx context.Context, g *MasterGrant) error {
	if g == nil {
		return fmt.Errorf("wallet: AuditedMasterGrantRepository.Create: grant is nil")
	}
	err := r.inner.Create(ctx, g)
	outcome := audit.OutcomeAllow
	if err != nil {
		outcome = audit.OutcomeDeny
	}
	ev := audit.MasterGrantIssued{
		GrantID:     g.ID(),
		Kind:        string(g.Kind()),
		TenantID:    g.TenantID(),
		ActorUserID: g.CreatedByUserID(),
		Reason:      g.Reason(),
		Amount:      amountFromPayload(g.Payload()),
		PeriodDays:  periodDaysFromPayload(g.Payload()),
		Outcome:     outcome,
		OccurredAt:  r.now().UTC(),
	}
	if werr := audit.WriteMasterGrantIssued(ctx, r.writer, ev); werr != nil {
		r.log.LogAttrs(ctx, slog.LevelWarn, "audit_master_grant_issued_write_failed",
			slog.String("grant_id", g.ID().String()),
			slog.String("tenant_id", g.TenantID().String()),
			slog.String("outcome", string(outcome)),
			slog.String("err", werr.Error()),
		)
	}
	return err
}

// GetByID delegates to inner.
func (r *AuditedMasterGrantRepository) GetByID(ctx context.Context, id uuid.UUID) (*MasterGrant, error) {
	return r.inner.GetByID(ctx, id)
}

// ListByTenant delegates to inner.
func (r *AuditedMasterGrantRepository) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*MasterGrant, error) {
	return r.inner.ListByTenant(ctx, tenantID)
}

// Revoke delegates to inner. The existing master_ops_audit trigger
// captures the revocation at the DB layer; a dedicated
// master.grant.revoked audit_log_security event is out of scope for
// SIN-62883 and will be added when the master revoke endpoint lands.
func (r *AuditedMasterGrantRepository) Revoke(ctx context.Context, id, revokedByUserID uuid.UUID, revokeReason string, now time.Time) error {
	return r.inner.Revoke(ctx, id, revokedByUserID, revokeReason, now)
}

var _ MasterGrantRepository = (*AuditedMasterGrantRepository)(nil)

// amountFromPayload pulls the "amount" int64 out of the grant payload
// for the audit row. Returns nil when absent or the wrong type — the
// JSON encoding (encoding/json) parses numbers into float64, so the
// helper accepts both int64 and float64 representations.
func amountFromPayload(p map[string]any) *int64 {
	if p == nil {
		return nil
	}
	v, ok := p["amount"]
	if !ok {
		return nil
	}
	if i, ok := coerceToInt64(v); ok {
		return &i
	}
	return nil
}

func periodDaysFromPayload(p map[string]any) *int {
	if p == nil {
		return nil
	}
	v, ok := p["period_days"]
	if !ok {
		return nil
	}
	if i, ok := coerceToInt64(v); ok {
		out := int(i)
		return &out
	}
	return nil
}

// coerceToInt64 normalises numeric JSON / native values to int64.
func coerceToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float32:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}
