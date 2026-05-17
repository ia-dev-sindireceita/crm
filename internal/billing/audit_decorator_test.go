package billing_test

// SIN-62883 / Fase 2.5 C8: assertions for the
// AuditedSubscriptionRepository and AuditedInvoiceRepository decorators.
// Pure (no DB): fake inner repositories + fake split-logger writer.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/iam/audit"
)

// --- fakes ------------------------------------------------------------

type fakeSubscriptionRepo struct {
	byTenant     map[uuid.UUID]*billing.Subscription
	getByTenant  func(uuid.UUID) (*billing.Subscription, error)
	saveCalls    int
	saveErr      error
	lastSavedID  uuid.UUID
	lastActor    uuid.UUID
	probeBoom    error // forces GetByTenant to return a non-ErrNotFound error
	probeBoomFor uuid.UUID
}

func (f *fakeSubscriptionRepo) GetByTenant(_ context.Context, tenantID uuid.UUID) (*billing.Subscription, error) {
	if f.getByTenant != nil {
		return f.getByTenant(tenantID)
	}
	if f.probeBoom != nil && tenantID == f.probeBoomFor {
		return nil, f.probeBoom
	}
	if s, ok := f.byTenant[tenantID]; ok {
		return s, nil
	}
	return nil, billing.ErrNotFound
}

func (f *fakeSubscriptionRepo) SaveSubscription(_ context.Context, s *billing.Subscription, actorID uuid.UUID) error {
	f.saveCalls++
	f.lastSavedID = s.ID()
	f.lastActor = actorID
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.byTenant == nil {
		f.byTenant = map[uuid.UUID]*billing.Subscription{}
	}
	f.byTenant[s.TenantID()] = s
	return nil
}

type fakeInvoiceRepo struct {
	byID         map[uuid.UUID]*billing.Invoice
	saveErr      error
	saveCalls    int
	probeBoom    error
	probeBoomFor uuid.UUID
}

func (f *fakeInvoiceRepo) GetByID(_ context.Context, tenantID, invoiceID uuid.UUID) (*billing.Invoice, error) {
	if f.probeBoom != nil && invoiceID == f.probeBoomFor {
		return nil, f.probeBoom
	}
	if inv, ok := f.byID[invoiceID]; ok {
		if inv.TenantID() != tenantID {
			return nil, billing.ErrNotFound
		}
		return inv, nil
	}
	return nil, billing.ErrNotFound
}

func (f *fakeInvoiceRepo) ListByTenant(_ context.Context, tenantID uuid.UUID) ([]*billing.Invoice, error) {
	var out []*billing.Invoice
	for _, inv := range f.byID {
		if inv.TenantID() == tenantID {
			out = append(out, inv)
		}
	}
	return out, nil
}

func (f *fakeInvoiceRepo) SaveInvoice(_ context.Context, inv *billing.Invoice, _ uuid.UUID) error {
	f.saveCalls++
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.byID == nil {
		f.byID = map[uuid.UUID]*billing.Invoice{}
	}
	f.byID[inv.ID()] = inv
	return nil
}

type recordingSplitLogger struct {
	security []audit.SecurityAuditEvent
}

func (l *recordingSplitLogger) WriteSecurity(_ context.Context, ev audit.SecurityAuditEvent) error {
	l.security = append(l.security, ev)
	return nil
}

func (l *recordingSplitLogger) WriteData(_ context.Context, _ audit.DataAuditEvent) error {
	return nil
}

// --- subscription -----------------------------------------------------

func TestAuditedSubscription_FirstSave_EmitsCreatedEvent(t *testing.T) {
	t.Parallel()
	inner := &fakeSubscriptionRepo{}
	writer := &recordingSplitLogger{}
	fixedNow := time.Date(2026, 5, 17, 14, 0, 0, 0, time.UTC)
	dec, err := billing.NewAuditedSubscriptionRepository(inner, writer, func() time.Time { return fixedNow }, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	tenant := uuid.New()
	plan := uuid.New()
	actor := uuid.New()
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	sub, err := billing.NewSubscription(tenant, plan, periodStart, periodEnd, time.Now())
	if err != nil {
		t.Fatalf("NewSubscription: %v", err)
	}
	if err := dec.SaveSubscription(context.Background(), sub, actor); err != nil {
		t.Fatalf("SaveSubscription: %v", err)
	}
	if inner.saveCalls != 1 {
		t.Fatalf("inner.SaveSubscription calls=%d, want 1", inner.saveCalls)
	}
	if len(writer.security) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(writer.security))
	}
	ev := writer.security[0]
	if ev.Event != audit.SecurityEventSubscriptionCreated {
		t.Fatalf("event=%q, want subscription.created", ev.Event)
	}
	if ev.ActorUserID != actor {
		t.Fatalf("actor=%q, want %q", ev.ActorUserID, actor)
	}
	if !ev.OccurredAt.Equal(fixedNow) {
		t.Fatalf("occurred=%v, want %v", ev.OccurredAt, fixedNow)
	}
	if ev.Target["outcome"] != "allow" {
		t.Fatalf("outcome=%v, want allow", ev.Target["outcome"])
	}
	if ev.Target["plan_id"] != plan.String() {
		t.Fatalf("plan_id=%v, want %q", ev.Target["plan_id"], plan)
	}
}

func TestAuditedSubscription_ExistingSubscription_DoesNotEmit(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	plan := uuid.New()
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	existing, err := billing.NewSubscription(tenant, plan, periodStart, periodEnd, time.Now())
	if err != nil {
		t.Fatalf("NewSubscription: %v", err)
	}
	inner := &fakeSubscriptionRepo{byTenant: map[uuid.UUID]*billing.Subscription{tenant: existing}}
	writer := &recordingSplitLogger{}
	dec, err := billing.NewAuditedSubscriptionRepository(inner, writer, nil, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	// Re-save the same subscription (renewal-style update) — same ID
	// as the existing row should NOT trip subscription.created.
	if err := dec.SaveSubscription(context.Background(), existing, uuid.New()); err != nil {
		t.Fatalf("SaveSubscription: %v", err)
	}
	if inner.saveCalls != 1 {
		t.Fatalf("inner.SaveSubscription calls=%d, want 1", inner.saveCalls)
	}
	if len(writer.security) != 0 {
		t.Fatalf("expected 0 audit rows on re-save of existing subscription, got %d", len(writer.security))
	}
}

func TestAuditedSubscription_NewIDForSameTenant_EmitsCreatedEvent(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	plan := uuid.New()
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	prev, err := billing.NewSubscription(tenant, plan, periodStart, periodEnd, time.Now())
	if err != nil {
		t.Fatalf("NewSubscription: %v", err)
	}
	inner := &fakeSubscriptionRepo{byTenant: map[uuid.UUID]*billing.Subscription{tenant: prev}}
	writer := &recordingSplitLogger{}
	dec, err := billing.NewAuditedSubscriptionRepository(inner, writer, nil, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	// New active subscription (different id) for same tenant — should
	// emit subscription.created because the row is fresh.
	next, err := billing.NewSubscription(tenant, uuid.New(), periodStart, periodEnd, time.Now())
	if err != nil {
		t.Fatalf("NewSubscription: %v", err)
	}
	if err := dec.SaveSubscription(context.Background(), next, uuid.New()); err != nil {
		t.Fatalf("SaveSubscription: %v", err)
	}
	if len(writer.security) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(writer.security))
	}
}

func TestAuditedSubscription_SaveError_StillEmitsDenyEvent(t *testing.T) {
	t.Parallel()
	boom := errors.New("postgres: rls denied subscription insert")
	inner := &fakeSubscriptionRepo{saveErr: boom}
	writer := &recordingSplitLogger{}
	dec, err := billing.NewAuditedSubscriptionRepository(inner, writer, nil, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	sub, err := billing.NewSubscription(uuid.New(), uuid.New(),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Now())
	if err != nil {
		t.Fatalf("NewSubscription: %v", err)
	}
	err = dec.SaveSubscription(context.Background(), sub, uuid.New())
	if !errors.Is(err, boom) {
		t.Fatalf("expected inner error to propagate, got %v", err)
	}
	if len(writer.security) != 1 {
		t.Fatalf("expected 1 deny audit row, got %d", len(writer.security))
	}
	if writer.security[0].Target["outcome"] != "deny" {
		t.Fatalf("outcome=%v, want deny", writer.security[0].Target["outcome"])
	}
}

func TestAuditedSubscription_ProbeFailure_SkipsAudit_StillSaves(t *testing.T) {
	t.Parallel()
	probeBoom := errors.New("postgres: rls fail on probe")
	tenant := uuid.New()
	inner := &fakeSubscriptionRepo{probeBoom: probeBoom, probeBoomFor: tenant}
	writer := &recordingSplitLogger{}
	dec, err := billing.NewAuditedSubscriptionRepository(inner, writer, nil, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	sub, err := billing.NewSubscription(tenant, uuid.New(),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Now())
	if err != nil {
		t.Fatalf("NewSubscription: %v", err)
	}
	if err := dec.SaveSubscription(context.Background(), sub, uuid.New()); err != nil {
		t.Fatalf("SaveSubscription: %v", err)
	}
	if inner.saveCalls != 1 {
		t.Fatalf("inner should still SaveSubscription on probe failure (=%d)", inner.saveCalls)
	}
	if len(writer.security) != 0 {
		t.Fatalf("audit must be skipped on probe failure to avoid spurious rows; got %d", len(writer.security))
	}
}

// --- invoice ----------------------------------------------------------

func TestAuditedInvoice_NonCancelTransition_SkipsAudit(t *testing.T) {
	t.Parallel()
	inner := &fakeInvoiceRepo{}
	writer := &recordingSplitLogger{}
	dec, err := billing.NewAuditedInvoiceRepository(inner, writer, nil, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	tenant := uuid.New()
	sub := uuid.New()
	inv, err := billing.NewInvoice(tenant, sub,
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 1000, time.Now())
	if err != nil {
		t.Fatalf("NewInvoice: %v", err)
	}
	if err := dec.SaveInvoice(context.Background(), inv, uuid.New()); err != nil {
		t.Fatalf("SaveInvoice: %v", err)
	}
	if inner.saveCalls != 1 {
		t.Fatalf("inner.SaveInvoice calls=%d, want 1", inner.saveCalls)
	}
	if len(writer.security) != 0 {
		t.Fatalf("non-cancel transition must not audit, got %d", len(writer.security))
	}
}

func TestAuditedInvoice_FirstCancel_EmitsEvent(t *testing.T) {
	t.Parallel()
	inner := &fakeInvoiceRepo{}
	writer := &recordingSplitLogger{}
	fixedNow := time.Date(2026, 5, 17, 16, 0, 0, 0, time.UTC)
	dec, err := billing.NewAuditedInvoiceRepository(inner, writer, func() time.Time { return fixedNow }, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	tenant := uuid.New()
	sub := uuid.New()
	actor := uuid.New()
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	inv, err := billing.NewInvoice(tenant, sub, periodStart, periodStart.AddDate(0, 1, 0), 1000, time.Now())
	if err != nil {
		t.Fatalf("NewInvoice: %v", err)
	}
	// Pre-seed the repo with a distinct hydrated copy in pending state
	// so the prior-state probe returns InvoiceStatePending. We cannot
	// reuse the `inv` pointer because CancelByMaster mutates it in
	// place and the probe would then observe the cancelled state.
	priorPending := billing.HydrateInvoice(inv.ID(), inv.TenantID(), inv.SubscriptionID(),
		inv.PeriodStart(), inv.PeriodEnd(), inv.AmountCentsBRL(),
		billing.InvoiceStatePending, "", inv.CreatedAt(), inv.UpdatedAt())
	inner.byID = map[uuid.UUID]*billing.Invoice{inv.ID(): priorPending}
	// Mutate the domain object to cancelled state.
	if err := inv.CancelByMaster("ten char reason for cancel", time.Now()); err != nil {
		t.Fatalf("CancelByMaster: %v", err)
	}
	if err := dec.SaveInvoice(context.Background(), inv, actor); err != nil {
		t.Fatalf("SaveInvoice: %v", err)
	}
	if len(writer.security) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(writer.security))
	}
	ev := writer.security[0]
	if ev.Event != audit.SecurityEventInvoiceCancelledByMaster {
		t.Fatalf("event=%q, want invoice.cancelled_by_master", ev.Event)
	}
	if !ev.OccurredAt.Equal(fixedNow) {
		t.Fatalf("occurred=%v, want %v", ev.OccurredAt, fixedNow)
	}
	if ev.Target["outcome"] != "allow" {
		t.Fatalf("outcome=%v, want allow", ev.Target["outcome"])
	}
	if ev.Target["reason"] != "ten char reason for cancel" {
		t.Fatalf("reason=%v", ev.Target["reason"])
	}
	if ev.Target["invoice_id"] != inv.ID().String() {
		t.Fatalf("invoice_id=%v, want %q", ev.Target["invoice_id"], inv.ID())
	}
}

func TestAuditedInvoice_RepeatedCancel_SkipsAudit(t *testing.T) {
	t.Parallel()
	inner := &fakeInvoiceRepo{}
	writer := &recordingSplitLogger{}
	dec, err := billing.NewAuditedInvoiceRepository(inner, writer, nil, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	tenant := uuid.New()
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	inv, err := billing.NewInvoice(tenant, uuid.New(), periodStart, periodStart.AddDate(0, 1, 0), 1000, time.Now())
	if err != nil {
		t.Fatalf("NewInvoice: %v", err)
	}
	if err := inv.CancelByMaster("ten char reason for cancel", time.Now()); err != nil {
		t.Fatalf("CancelByMaster: %v", err)
	}
	// Pre-seed: prior-state probe will return cancelled.
	inner.byID = map[uuid.UUID]*billing.Invoice{inv.ID(): inv}
	if err := dec.SaveInvoice(context.Background(), inv, uuid.New()); err != nil {
		t.Fatalf("SaveInvoice: %v", err)
	}
	if len(writer.security) != 0 {
		t.Fatalf("idempotent re-save of already-cancelled invoice must not audit, got %d", len(writer.security))
	}
}

func TestAuditedInvoice_CancelDenyPath_StillAudits(t *testing.T) {
	t.Parallel()
	boom := errors.New("postgres: rls denied cancel write")
	inner := &fakeInvoiceRepo{saveErr: boom}
	writer := &recordingSplitLogger{}
	dec, err := billing.NewAuditedInvoiceRepository(inner, writer, nil, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	tenant := uuid.New()
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	inv, err := billing.NewInvoice(tenant, uuid.New(), periodStart, periodStart.AddDate(0, 1, 0), 1000, time.Now())
	if err != nil {
		t.Fatalf("NewInvoice: %v", err)
	}
	if err := inv.CancelByMaster("ten char reason for cancel", time.Now()); err != nil {
		t.Fatalf("CancelByMaster: %v", err)
	}
	// No pre-seed → probe returns ErrNotFound → priorCancelled=false → audit fires.
	err = dec.SaveInvoice(context.Background(), inv, uuid.New())
	if !errors.Is(err, boom) {
		t.Fatalf("expected inner error to propagate, got %v", err)
	}
	if len(writer.security) != 1 {
		t.Fatalf("expected 1 deny audit row, got %d", len(writer.security))
	}
	if writer.security[0].Target["outcome"] != "deny" {
		t.Fatalf("outcome=%v, want deny", writer.security[0].Target["outcome"])
	}
}

func TestAuditedInvoice_ProbeFailure_SkipsAudit_StillSaves(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	inv, err := billing.NewInvoice(tenant, uuid.New(),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 1000, time.Now())
	if err != nil {
		t.Fatalf("NewInvoice: %v", err)
	}
	if err := inv.CancelByMaster("ten char reason for cancel", time.Now()); err != nil {
		t.Fatalf("CancelByMaster: %v", err)
	}
	probeBoom := errors.New("postgres: rls fail on probe")
	inner := &fakeInvoiceRepo{probeBoom: probeBoom, probeBoomFor: inv.ID()}
	writer := &recordingSplitLogger{}
	dec, err := billing.NewAuditedInvoiceRepository(inner, writer, nil, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if err := dec.SaveInvoice(context.Background(), inv, uuid.New()); err != nil {
		t.Fatalf("SaveInvoice: %v", err)
	}
	if inner.saveCalls != 1 {
		t.Fatalf("inner.SaveInvoice should still be called on probe failure (=%d)", inner.saveCalls)
	}
	if len(writer.security) != 0 {
		t.Fatalf("audit must be skipped on probe failure to avoid spurious rows; got %d", len(writer.security))
	}
}

func TestAuditedInvoice_Constructor_RejectsNil(t *testing.T) {
	t.Parallel()
	writer := &recordingSplitLogger{}
	if _, err := billing.NewAuditedInvoiceRepository(nil, writer, nil, nil); err == nil {
		t.Fatal("expected error on nil inner")
	}
	if _, err := billing.NewAuditedInvoiceRepository(&fakeInvoiceRepo{}, nil, nil, nil); err == nil {
		t.Fatal("expected error on nil writer")
	}
}

func TestAuditedSubscription_Constructor_RejectsNil(t *testing.T) {
	t.Parallel()
	writer := &recordingSplitLogger{}
	if _, err := billing.NewAuditedSubscriptionRepository(nil, writer, nil, nil); err == nil {
		t.Fatal("expected error on nil inner")
	}
	if _, err := billing.NewAuditedSubscriptionRepository(&fakeSubscriptionRepo{}, nil, nil, nil); err == nil {
		t.Fatal("expected error on nil writer")
	}
}
