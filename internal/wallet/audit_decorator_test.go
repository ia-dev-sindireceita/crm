package wallet_test

// SIN-62883 / Fase 2.5 C8: assertions for the AuditedMasterGrantRepository
// decorator. Pure (no DB): fake inner repo + fake writer to keep the
// behaviour observable without a postgres dependency.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/wallet"
)

type fakeMasterGrantRepo struct {
	createErr      error
	createCalls    int
	lastGrantID    uuid.UUID
	getByIDStub    map[uuid.UUID]*wallet.MasterGrant
	listByTenant   func(uuid.UUID) []*wallet.MasterGrant
	revokeErr      error
	lastRevokedID  uuid.UUID
	lastRevokedBy  uuid.UUID
	lastRevokeWhen time.Time
}

func (f *fakeMasterGrantRepo) Create(_ context.Context, g *wallet.MasterGrant) error {
	f.createCalls++
	f.lastGrantID = g.ID()
	return f.createErr
}

func (f *fakeMasterGrantRepo) GetByID(_ context.Context, id uuid.UUID) (*wallet.MasterGrant, error) {
	if g, ok := f.getByIDStub[id]; ok {
		return g, nil
	}
	return nil, wallet.ErrNotFound
}

func (f *fakeMasterGrantRepo) ListByTenant(_ context.Context, tenantID uuid.UUID) ([]*wallet.MasterGrant, error) {
	if f.listByTenant != nil {
		return f.listByTenant(tenantID), nil
	}
	return nil, nil
}

func (f *fakeMasterGrantRepo) Revoke(_ context.Context, id, by uuid.UUID, _ string, when time.Time) error {
	f.lastRevokedID = id
	f.lastRevokedBy = by
	f.lastRevokeWhen = when
	return f.revokeErr
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

func TestAuditedMasterGrantRepository_Create_AllowPath(t *testing.T) {
	t.Parallel()
	inner := &fakeMasterGrantRepo{}
	writer := &recordingSplitLogger{}
	fixedNow := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	decorator, err := wallet.NewAuditedMasterGrantRepository(inner, writer, func() time.Time { return fixedNow }, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}

	tenant := uuid.New()
	actor := uuid.New()
	periodDays := 30
	g, err := wallet.NewMasterGrant(tenant, actor, wallet.KindFreeSubscriptionPeriod, map[string]any{
		"period_days": periodDays,
	}, "trial extension approved", time.Now())
	if err != nil {
		t.Fatalf("NewMasterGrant: %v", err)
	}

	if err := decorator.Create(context.Background(), g); err != nil {
		t.Fatalf("decorator.Create: %v", err)
	}
	if inner.createCalls != 1 {
		t.Fatalf("inner.Create calls=%d, want 1", inner.createCalls)
	}
	if inner.lastGrantID != g.ID() {
		t.Fatalf("inner saw grant id %q, want %q", inner.lastGrantID, g.ID())
	}
	if len(writer.security) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(writer.security))
	}
	ev := writer.security[0]
	if ev.Event != audit.SecurityEventMasterGrantIssued {
		t.Fatalf("event=%q, want %q", ev.Event, audit.SecurityEventMasterGrantIssued)
	}
	if ev.ActorUserID != actor {
		t.Fatalf("actor=%q, want %q", ev.ActorUserID, actor)
	}
	if ev.TenantID == nil || *ev.TenantID != tenant {
		t.Fatalf("tenant=%v, want %q", ev.TenantID, tenant)
	}
	if !ev.OccurredAt.Equal(fixedNow) {
		t.Fatalf("occurred=%v, want %v (decorator now func)", ev.OccurredAt, fixedNow)
	}
	if ev.Target["outcome"] != "allow" {
		t.Fatalf("outcome=%v, want allow", ev.Target["outcome"])
	}
	if ev.Target["kind"] != "free_subscription_period" {
		t.Fatalf("kind=%v, want free_subscription_period", ev.Target["kind"])
	}
	if ev.Target["period_days"] != 30 {
		t.Fatalf("period_days=%v, want 30", ev.Target["period_days"])
	}
	if _, ok := ev.Target["amount"]; ok {
		t.Fatalf("amount should be absent when grant kind is free_subscription_period: %+v", ev.Target)
	}
}

func TestAuditedMasterGrantRepository_Create_DenyPath_StillAudits(t *testing.T) {
	t.Parallel()
	boom := errors.New("postgres: insert master_grant: rls denied")
	inner := &fakeMasterGrantRepo{createErr: boom}
	writer := &recordingSplitLogger{}
	decorator, err := wallet.NewAuditedMasterGrantRepository(inner, writer, nil, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	g, err := wallet.NewMasterGrant(uuid.New(), uuid.New(), wallet.KindExtraTokens,
		map[string]any{"amount": int64(100)}, "deny path test reason", time.Now())
	if err != nil {
		t.Fatalf("NewMasterGrant: %v", err)
	}
	err = decorator.Create(context.Background(), g)
	if !errors.Is(err, boom) {
		t.Fatalf("expected inner error to propagate, got %v", err)
	}
	if len(writer.security) != 1 {
		t.Fatalf("expected 1 audit row even on deny, got %d", len(writer.security))
	}
	if writer.security[0].Target["outcome"] != "deny" {
		t.Fatalf("outcome=%v, want deny", writer.security[0].Target["outcome"])
	}
	if writer.security[0].Target["amount"] != int64(100) {
		t.Fatalf("amount=%v, want 100", writer.security[0].Target["amount"])
	}
}

func TestAuditedMasterGrantRepository_Constructor_RejectsNil(t *testing.T) {
	t.Parallel()
	writer := &recordingSplitLogger{}
	if _, err := wallet.NewAuditedMasterGrantRepository(nil, writer, nil, nil); err == nil {
		t.Fatal("expected error on nil inner")
	}
	if _, err := wallet.NewAuditedMasterGrantRepository(&fakeMasterGrantRepo{}, nil, nil, nil); err == nil {
		t.Fatal("expected error on nil writer")
	}
}

func TestAuditedMasterGrantRepository_DelegatesReadsAndRevoke(t *testing.T) {
	t.Parallel()
	g, err := wallet.NewMasterGrant(uuid.New(), uuid.New(), wallet.KindExtraTokens,
		map[string]any{"amount": int64(10)}, "valid reason for tests", time.Now())
	if err != nil {
		t.Fatalf("NewMasterGrant: %v", err)
	}
	inner := &fakeMasterGrantRepo{
		getByIDStub:  map[uuid.UUID]*wallet.MasterGrant{g.ID(): g},
		listByTenant: func(uuid.UUID) []*wallet.MasterGrant { return []*wallet.MasterGrant{g} },
	}
	writer := &recordingSplitLogger{}
	decorator, err := wallet.NewAuditedMasterGrantRepository(inner, writer, nil, nil)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}

	got, err := decorator.GetByID(context.Background(), g.ID())
	if err != nil || got == nil || got.ID() != g.ID() {
		t.Fatalf("GetByID delegate broken: got=%v err=%v", got, err)
	}
	list, err := decorator.ListByTenant(context.Background(), g.TenantID())
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByTenant delegate broken: list=%v err=%v", list, err)
	}
	now := time.Now()
	if err := decorator.Revoke(context.Background(), g.ID(), uuid.New(), "ten char reason", now); err != nil {
		t.Fatalf("Revoke delegate broken: %v", err)
	}
	if inner.lastRevokedID != g.ID() {
		t.Fatalf("Revoke did not delegate id: got=%q want=%q", inner.lastRevokedID, g.ID())
	}
	if len(writer.security) != 0 {
		t.Fatalf("Revoke must not emit master.grant.issued; got %d rows", len(writer.security))
	}
}
