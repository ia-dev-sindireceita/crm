package audit

// SIN-62883 / Fase 2.5 C8: writer helpers for the three master
// billing/wallet event types. The helpers keep the payload shape
// canonical so every call site emits an audit_log_security row with
// the same JSON contract — master-console queries and downstream
// SIEM rules can rely on stable field names.
//
// Each helper:
//
//   - Validates that the actor and tenant are non-zero (boundary
//     guard — SplitLogger also rejects, but failing here keeps the
//     stack trace at the caller).
//   - Builds the canonical target map. Optional fields are only set
//     when the caller supplied a non-nil/non-empty value, so the
//     JSON wire shape stays minimal and assertable.
//   - Stamps Outcome ∈ {"allow","deny"} on the target so the
//     master-console UI can render successful vs rejected attempts
//     consistently (mirrors the authz_allow/deny convention).
//   - Forwards to SplitLogger.WriteSecurity. Returns the writer
//     error unchanged so the caller can fail closed (non-repudiation
//     contract — same as every other audit hook in the system).
//
// Payload field names match the contract in the SIN-62883 issue
// description; renaming a field is a wire-breaking change.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Outcome is the success/rejection axis recorded alongside every
// billing/wallet audit row. Mirrors the authz_allow/authz_deny
// convention so master-console queries can union the two ledgers.
type Outcome string

const (
	OutcomeAllow Outcome = "allow"
	OutcomeDeny  Outcome = "deny"
)

// ErrInvalidBillingAuditEvent signals that a billing/wallet audit
// helper was called with a missing required field. Distinct from a
// writer error so callers can short-circuit retries — the underlying
// data is malformed, not the network.
var ErrInvalidBillingAuditEvent = errors.New("audit: invalid billing audit event")

// MasterGrantIssued is the structured payload for a master.grant.issued
// event. Amount and PeriodDays are kind-specific — Amount is set for
// KindExtraTokens grants, PeriodDays for KindFreeSubscriptionPeriod.
// Reason mirrors the master_grant.reason column (≥10 chars enforced
// upstream by the domain object); empty Reason is rejected.
type MasterGrantIssued struct {
	GrantID     uuid.UUID
	Kind        string
	TenantID    uuid.UUID
	ActorUserID uuid.UUID
	Reason      string
	Amount      *int64
	PeriodDays  *int
	Outcome     Outcome
	OccurredAt  time.Time
}

// SubscriptionCreated is the structured payload for a
// subscription.created event. The actor is the master user who
// triggered the onboarding/plan change; the period start is the
// human-readable anchor that the master console renders alongside the
// plan badge.
type SubscriptionCreated struct {
	SubscriptionID     uuid.UUID
	TenantID           uuid.UUID
	PlanID             uuid.UUID
	CurrentPeriodStart time.Time
	ActorUserID        uuid.UUID
	Outcome            Outcome
	OccurredAt         time.Time
}

// InvoiceCancelledByMaster is the structured payload for an
// invoice.cancelled_by_master event. Reason is mandatory because the
// underlying invoice.cancelled_reason CHECK requires ≥10 chars; the
// audit row preserves the same reason so master-console reviewers can
// reconstruct why an invoice was voided without re-querying the
// invoice table.
type InvoiceCancelledByMaster struct {
	InvoiceID   uuid.UUID
	TenantID    uuid.UUID
	PeriodStart time.Time
	Reason      string
	ActorUserID uuid.UUID
	Outcome     Outcome
	OccurredAt  time.Time
}

// WriteMasterGrantIssued writes one master.grant.issued row to
// audit_log_security via writer.
func WriteMasterGrantIssued(ctx context.Context, writer SplitLogger, e MasterGrantIssued) error {
	if writer == nil {
		return ErrInvalidBillingAuditEvent
	}
	if e.ActorUserID == uuid.Nil {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("zero actor user id"))
	}
	if e.TenantID == uuid.Nil {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("zero tenant id"))
	}
	if e.GrantID == uuid.Nil {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("zero grant id"))
	}
	if e.Kind == "" {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("empty grant kind"))
	}
	if e.Reason == "" {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("empty grant reason"))
	}
	outcome := e.Outcome
	if outcome == "" {
		outcome = OutcomeAllow
	}
	target := map[string]any{
		"outcome":       string(outcome),
		"grant_id":      e.GrantID.String(),
		"kind":          e.Kind,
		"tenant_id":     e.TenantID.String(),
		"actor_user_id": e.ActorUserID.String(),
		"reason":        e.Reason,
	}
	if e.Amount != nil {
		target["amount"] = *e.Amount
	}
	if e.PeriodDays != nil {
		target["period_days"] = *e.PeriodDays
	}
	tenantID := e.TenantID
	return writer.WriteSecurity(ctx, SecurityAuditEvent{
		Event:       SecurityEventMasterGrantIssued,
		ActorUserID: e.ActorUserID,
		TenantID:    &tenantID,
		Target:      target,
		OccurredAt:  e.OccurredAt,
	})
}

// WriteSubscriptionCreated writes one subscription.created row to
// audit_log_security via writer.
func WriteSubscriptionCreated(ctx context.Context, writer SplitLogger, e SubscriptionCreated) error {
	if writer == nil {
		return ErrInvalidBillingAuditEvent
	}
	if e.ActorUserID == uuid.Nil {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("zero actor user id"))
	}
	if e.TenantID == uuid.Nil {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("zero tenant id"))
	}
	if e.SubscriptionID == uuid.Nil {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("zero subscription id"))
	}
	if e.PlanID == uuid.Nil {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("zero plan id"))
	}
	outcome := e.Outcome
	if outcome == "" {
		outcome = OutcomeAllow
	}
	target := map[string]any{
		"outcome":              string(outcome),
		"subscription_id":      e.SubscriptionID.String(),
		"tenant_id":            e.TenantID.String(),
		"plan_id":              e.PlanID.String(),
		"current_period_start": e.CurrentPeriodStart.UTC().Format(time.RFC3339Nano),
		"actor_user_id":        e.ActorUserID.String(),
	}
	tenantID := e.TenantID
	return writer.WriteSecurity(ctx, SecurityAuditEvent{
		Event:       SecurityEventSubscriptionCreated,
		ActorUserID: e.ActorUserID,
		TenantID:    &tenantID,
		Target:      target,
		OccurredAt:  e.OccurredAt,
	})
}

// WriteInvoiceCancelledByMaster writes one
// invoice.cancelled_by_master row to audit_log_security via writer.
func WriteInvoiceCancelledByMaster(ctx context.Context, writer SplitLogger, e InvoiceCancelledByMaster) error {
	if writer == nil {
		return ErrInvalidBillingAuditEvent
	}
	if e.ActorUserID == uuid.Nil {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("zero actor user id"))
	}
	if e.TenantID == uuid.Nil {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("zero tenant id"))
	}
	if e.InvoiceID == uuid.Nil {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("zero invoice id"))
	}
	if e.Reason == "" {
		return errors.Join(ErrInvalidBillingAuditEvent, errors.New("empty cancel reason"))
	}
	outcome := e.Outcome
	if outcome == "" {
		outcome = OutcomeAllow
	}
	target := map[string]any{
		"outcome":       string(outcome),
		"invoice_id":    e.InvoiceID.String(),
		"tenant_id":     e.TenantID.String(),
		"period_start":  e.PeriodStart.UTC().Format(time.RFC3339Nano),
		"reason":        e.Reason,
		"actor_user_id": e.ActorUserID.String(),
	}
	tenantID := e.TenantID
	return writer.WriteSecurity(ctx, SecurityAuditEvent{
		Event:       SecurityEventInvoiceCancelledByMaster,
		ActorUserID: e.ActorUserID,
		TenantID:    &tenantID,
		Target:      target,
		OccurredAt:  e.OccurredAt,
	})
}
