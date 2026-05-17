package billing

// SIN-62883 / Fase 2.5 C8: audit decorators over SubscriptionRepository
// and InvoiceRepository.
//
// Each decorator wraps an inner repository and emits one
// audit_log_security row per master-actor mutation:
//
//   * SaveSubscription that resulted in a fresh INSERT → one
//     subscription.created event. The decorator distinguishes "create"
//     from "update" by checking the inner repository for an existing
//     subscription on the same id before delegating; on first call
//     ErrNotFound is returned and we know the upcoming Save is a
//     create. Subsequent saves (renewals, plan changes) skip the audit
//     hook because subscription.created is a one-shot event per
//     subscription row — the master_ops_audit trigger captures the
//     update.
//
//   * SaveInvoice that transitioned to InvoiceStateCancelledByMaster
//     → one invoice.cancelled_by_master event. Same probe-before-save
//     pattern: if the prior state was already cancelled_by_master we
//     skip; otherwise the transition is auditable. Pending→paid and
//     other non-cancellation transitions are not audited by this
//     decorator.
//
// Audit writes are best-effort and warn-logged on failure (the row
// has already been persisted; the use case must remain idempotent).
// Outcome=allow on success of the inner write; outcome=deny when the
// inner write returns an error.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

// AuditedSubscriptionRepository decorates a SubscriptionRepository
// with a synchronous audit hook on SaveSubscription. Only the first
// Save for a given subscription id emits subscription.created — the
// decorator probes the inner repo via a tenant-scoped lookup to
// distinguish create from update.
type AuditedSubscriptionRepository struct {
	inner  SubscriptionRepository
	writer audit.SplitLogger
	now    func() time.Time
	log    *slog.Logger
}

// NewAuditedSubscriptionRepository wires the decorator.
func NewAuditedSubscriptionRepository(inner SubscriptionRepository, writer audit.SplitLogger, now func() time.Time, log *slog.Logger) (*AuditedSubscriptionRepository, error) {
	if inner == nil {
		return nil, fmt.Errorf("billing: AuditedSubscriptionRepository: inner repository is nil")
	}
	if writer == nil {
		return nil, fmt.Errorf("billing: AuditedSubscriptionRepository: audit writer is nil")
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &AuditedSubscriptionRepository{inner: inner, writer: writer, now: now, log: log}, nil
}

// GetByTenant delegates to inner.
func (r *AuditedSubscriptionRepository) GetByTenant(ctx context.Context, tenantID uuid.UUID) (*Subscription, error) {
	return r.inner.GetByTenant(ctx, tenantID)
}

// SaveSubscription delegates to inner. When the prior state of the
// row is "does not exist for this tenant" (ErrNotFound on the
// tenant-scoped lookup), the save is treated as a create and a
// subscription.created audit row is written. Existing-row updates are
// not audited at this layer.
func (r *AuditedSubscriptionRepository) SaveSubscription(ctx context.Context, s *Subscription, actorID uuid.UUID) error {
	if s == nil {
		return fmt.Errorf("billing: AuditedSubscriptionRepository.SaveSubscription: subscription is nil")
	}
	isCreate, probeErr := r.isCreate(ctx, s)
	if probeErr != nil {
		// The probe is best-effort; if it fails we still let the
		// underlying save proceed. We do NOT audit because we cannot
		// tell create from update, and writing a spurious
		// subscription.created row would poison the master console
		// query that counts onboarding events.
		r.log.LogAttrs(ctx, slog.LevelWarn, "audit_subscription_create_probe_failed",
			slog.String("subscription_id", s.ID().String()),
			slog.String("tenant_id", s.TenantID().String()),
			slog.String("err", probeErr.Error()),
		)
		return r.inner.SaveSubscription(ctx, s, actorID)
	}
	saveErr := r.inner.SaveSubscription(ctx, s, actorID)
	if !isCreate {
		return saveErr
	}
	outcome := audit.OutcomeAllow
	if saveErr != nil {
		outcome = audit.OutcomeDeny
	}
	ev := audit.SubscriptionCreated{
		SubscriptionID:     s.ID(),
		TenantID:           s.TenantID(),
		PlanID:             s.PlanID(),
		CurrentPeriodStart: s.CurrentPeriodStart(),
		ActorUserID:        actorID,
		Outcome:            outcome,
		OccurredAt:         r.now().UTC(),
	}
	if werr := audit.WriteSubscriptionCreated(ctx, r.writer, ev); werr != nil {
		r.log.LogAttrs(ctx, slog.LevelWarn, "audit_subscription_created_write_failed",
			slog.String("subscription_id", s.ID().String()),
			slog.String("tenant_id", s.TenantID().String()),
			slog.String("outcome", string(outcome)),
			slog.String("err", werr.Error()),
		)
	}
	return saveErr
}

// isCreate probes the inner repository to decide whether a Save is a
// fresh insert. The tenant must already exist for the lookup to
// succeed — a brand-new tenant returns ErrNotFound which we map to
// create. Any other error is surfaced so the caller can choose to
// skip auditing.
func (r *AuditedSubscriptionRepository) isCreate(ctx context.Context, s *Subscription) (bool, error) {
	existing, err := r.inner.GetByTenant(ctx, s.TenantID())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return true, nil
		}
		return false, err
	}
	// A row exists for this tenant; it's a create only if the active
	// row has a different id (e.g. previous subscription cancelled
	// and a new active one is being inserted).
	return existing.ID() != s.ID(), nil
}

var _ SubscriptionRepository = (*AuditedSubscriptionRepository)(nil)

// AuditedInvoiceRepository decorates an InvoiceRepository with an
// audit hook on SaveInvoice that fires only on the transition to
// InvoiceStateCancelledByMaster.
type AuditedInvoiceRepository struct {
	inner  InvoiceRepository
	writer audit.SplitLogger
	now    func() time.Time
	log    *slog.Logger
}

// NewAuditedInvoiceRepository wires the decorator.
func NewAuditedInvoiceRepository(inner InvoiceRepository, writer audit.SplitLogger, now func() time.Time, log *slog.Logger) (*AuditedInvoiceRepository, error) {
	if inner == nil {
		return nil, fmt.Errorf("billing: AuditedInvoiceRepository: inner repository is nil")
	}
	if writer == nil {
		return nil, fmt.Errorf("billing: AuditedInvoiceRepository: audit writer is nil")
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &AuditedInvoiceRepository{inner: inner, writer: writer, now: now, log: log}, nil
}

// GetByID delegates to inner.
func (r *AuditedInvoiceRepository) GetByID(ctx context.Context, tenantID, invoiceID uuid.UUID) (*Invoice, error) {
	return r.inner.GetByID(ctx, tenantID, invoiceID)
}

// ListByTenant delegates to inner.
func (r *AuditedInvoiceRepository) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*Invoice, error) {
	return r.inner.ListByTenant(ctx, tenantID)
}

// SaveInvoice delegates to inner. When the new state is
// InvoiceStateCancelledByMaster and the prior state (probed via
// GetByID) was anything else, an invoice.cancelled_by_master audit
// row is written. Repeated saves with the same cancelled state are
// not audited (idempotent re-saves should not double-log).
func (r *AuditedInvoiceRepository) SaveInvoice(ctx context.Context, inv *Invoice, actorID uuid.UUID) error {
	if inv == nil {
		return fmt.Errorf("billing: AuditedInvoiceRepository.SaveInvoice: invoice is nil")
	}
	if inv.State() != InvoiceStateCancelledByMaster {
		return r.inner.SaveInvoice(ctx, inv, actorID)
	}
	priorCancelled, probeErr := r.priorStateCancelled(ctx, inv)
	if probeErr != nil {
		r.log.LogAttrs(ctx, slog.LevelWarn, "audit_invoice_cancel_probe_failed",
			slog.String("invoice_id", inv.ID().String()),
			slog.String("tenant_id", inv.TenantID().String()),
			slog.String("err", probeErr.Error()),
		)
		return r.inner.SaveInvoice(ctx, inv, actorID)
	}
	saveErr := r.inner.SaveInvoice(ctx, inv, actorID)
	if priorCancelled {
		// Already cancelled; skip auditing on idempotent re-save.
		return saveErr
	}
	outcome := audit.OutcomeAllow
	if saveErr != nil {
		outcome = audit.OutcomeDeny
	}
	ev := audit.InvoiceCancelledByMaster{
		InvoiceID:   inv.ID(),
		TenantID:    inv.TenantID(),
		PeriodStart: inv.PeriodStart(),
		Reason:      inv.CancelledReason(),
		ActorUserID: actorID,
		Outcome:     outcome,
		OccurredAt:  r.now().UTC(),
	}
	if werr := audit.WriteInvoiceCancelledByMaster(ctx, r.writer, ev); werr != nil {
		r.log.LogAttrs(ctx, slog.LevelWarn, "audit_invoice_cancelled_write_failed",
			slog.String("invoice_id", inv.ID().String()),
			slog.String("tenant_id", inv.TenantID().String()),
			slog.String("outcome", string(outcome)),
			slog.String("err", werr.Error()),
		)
	}
	return saveErr
}

// priorStateCancelled probes the inner repo to decide whether the
// row was already cancelled. ErrNotFound means this is a fresh insert
// — in that case priorCancelled is false (we'll write the audit row
// for the first cancellation).
func (r *AuditedInvoiceRepository) priorStateCancelled(ctx context.Context, inv *Invoice) (bool, error) {
	existing, err := r.inner.GetByID(ctx, inv.TenantID(), inv.ID())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return existing.State() == InvoiceStateCancelledByMaster, nil
}

var _ InvoiceRepository = (*AuditedInvoiceRepository)(nil)
