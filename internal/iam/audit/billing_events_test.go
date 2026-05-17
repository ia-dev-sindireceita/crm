package audit_test

// SIN-62883 / Fase 2.5 C8: assertions for the master billing/wallet
// audit-writer helpers. Pure (no DB); the postgres adapter has its own
// integration tests in internal/adapter/db/postgres.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

// recordingSplitLogger is a thread-unsafe fake SplitLogger that captures
// every call so tests can assert wire-shape invariants.
type recordingSplitLogger struct {
	security []audit.SecurityAuditEvent
	data     []audit.DataAuditEvent
	failWith error
}

func (l *recordingSplitLogger) WriteSecurity(_ context.Context, ev audit.SecurityAuditEvent) error {
	if l.failWith != nil {
		return l.failWith
	}
	l.security = append(l.security, ev)
	return nil
}

func (l *recordingSplitLogger) WriteData(_ context.Context, ev audit.DataAuditEvent) error {
	if l.failWith != nil {
		return l.failWith
	}
	l.data = append(l.data, ev)
	return nil
}

func TestSecurityEvent_BillingStableNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got, want string
	}{
		{string(audit.SecurityEventMasterGrantIssued), "master.grant.issued"},
		{string(audit.SecurityEventSubscriptionCreated), "subscription.created"},
		{string(audit.SecurityEventInvoiceCancelledByMaster), "invoice.cancelled_by_master"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("security event constant mismatch: got %q, want %q — wire-stable, mirrors migration 0100 CHECK", tc.got, tc.want)
		}
	}
}

func TestSecurityEvent_BillingIsKnown(t *testing.T) {
	t.Parallel()
	known := []audit.SecurityEvent{
		audit.SecurityEventMasterGrantIssued,
		audit.SecurityEventSubscriptionCreated,
		audit.SecurityEventInvoiceCancelledByMaster,
	}
	for _, e := range known {
		if !e.IsKnown() {
			t.Fatalf("SecurityEvent(%q).IsKnown()=false, want true — IsKnown map must list every billing/wallet event", e)
		}
	}
}

func TestWriteMasterGrantIssued_AllowPath(t *testing.T) {
	t.Parallel()
	writer := &recordingSplitLogger{}
	grantID := uuid.New()
	tenantID := uuid.New()
	actorID := uuid.New()
	amount := int64(50000)
	occurred := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)

	err := audit.WriteMasterGrantIssued(context.Background(), writer, audit.MasterGrantIssued{
		GrantID:     grantID,
		Kind:        "extra_tokens",
		TenantID:    tenantID,
		ActorUserID: actorID,
		Reason:      "approved by ceo for q2 onboarding",
		Amount:      &amount,
		Outcome:     audit.OutcomeAllow,
		OccurredAt:  occurred,
	})
	if err != nil {
		t.Fatalf("WriteMasterGrantIssued unexpected error: %v", err)
	}
	if len(writer.security) != 1 {
		t.Fatalf("expected 1 audit_log_security row, got %d", len(writer.security))
	}
	ev := writer.security[0]
	if ev.Event != audit.SecurityEventMasterGrantIssued {
		t.Fatalf("event=%q, want %q", ev.Event, audit.SecurityEventMasterGrantIssued)
	}
	if ev.ActorUserID != actorID {
		t.Fatalf("actor=%q, want %q", ev.ActorUserID, actorID)
	}
	if ev.TenantID == nil || *ev.TenantID != tenantID {
		t.Fatalf("tenant=%v, want %q", ev.TenantID, tenantID)
	}
	if !ev.OccurredAt.Equal(occurred) {
		t.Fatalf("occurred_at=%v, want %v", ev.OccurredAt, occurred)
	}
	if ev.Target["outcome"] != "allow" {
		t.Fatalf("target.outcome=%v, want allow", ev.Target["outcome"])
	}
	if ev.Target["grant_id"] != grantID.String() {
		t.Fatalf("target.grant_id=%v, want %q", ev.Target["grant_id"], grantID)
	}
	if ev.Target["kind"] != "extra_tokens" {
		t.Fatalf("target.kind=%v, want extra_tokens", ev.Target["kind"])
	}
	if ev.Target["amount"] != int64(50000) {
		t.Fatalf("target.amount=%v, want 50000", ev.Target["amount"])
	}
	if _, ok := ev.Target["period_days"]; ok {
		t.Fatalf("target.period_days should be absent when Amount-style grant: %+v", ev.Target)
	}
}

func TestWriteMasterGrantIssued_DenyPath(t *testing.T) {
	t.Parallel()
	writer := &recordingSplitLogger{}
	periodDays := 30

	err := audit.WriteMasterGrantIssued(context.Background(), writer, audit.MasterGrantIssued{
		GrantID:     uuid.New(),
		Kind:        "free_subscription_period",
		TenantID:    uuid.New(),
		ActorUserID: uuid.New(),
		Reason:      "ops trial extension, deny path test",
		PeriodDays:  &periodDays,
		Outcome:     audit.OutcomeDeny,
	})
	if err != nil {
		t.Fatalf("WriteMasterGrantIssued unexpected error: %v", err)
	}
	if len(writer.security) != 1 {
		t.Fatalf("expected 1 row, got %d", len(writer.security))
	}
	if writer.security[0].Target["outcome"] != "deny" {
		t.Fatalf("target.outcome=%v, want deny", writer.security[0].Target["outcome"])
	}
	if writer.security[0].Target["period_days"] != 30 {
		t.Fatalf("target.period_days=%v, want 30", writer.security[0].Target["period_days"])
	}
}

func TestWriteMasterGrantIssued_RejectsZeroFields(t *testing.T) {
	t.Parallel()
	base := audit.MasterGrantIssued{
		GrantID:     uuid.New(),
		Kind:        "extra_tokens",
		TenantID:    uuid.New(),
		ActorUserID: uuid.New(),
		Reason:      "valid reason >=10 chars",
	}
	writer := &recordingSplitLogger{}
	tests := []struct {
		name  string
		mut   func(*audit.MasterGrantIssued)
		match string
	}{
		{"zero actor", func(e *audit.MasterGrantIssued) { e.ActorUserID = uuid.Nil }, "actor"},
		{"zero tenant", func(e *audit.MasterGrantIssued) { e.TenantID = uuid.Nil }, "tenant"},
		{"zero grant", func(e *audit.MasterGrantIssued) { e.GrantID = uuid.Nil }, "grant"},
		{"empty kind", func(e *audit.MasterGrantIssued) { e.Kind = "" }, "kind"},
		{"empty reason", func(e *audit.MasterGrantIssued) { e.Reason = "" }, "reason"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := base
			tc.mut(&ev)
			err := audit.WriteMasterGrantIssued(context.Background(), writer, ev)
			if err == nil {
				t.Fatal("expected error for invalid field")
			}
			if !errors.Is(err, audit.ErrInvalidBillingAuditEvent) {
				t.Fatalf("expected ErrInvalidBillingAuditEvent, got %v", err)
			}
		})
	}
}

func TestWriteSubscriptionCreated_AllowPath(t *testing.T) {
	t.Parallel()
	writer := &recordingSplitLogger{}
	subID := uuid.New()
	tenantID := uuid.New()
	planID := uuid.New()
	actorID := uuid.New()
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	err := audit.WriteSubscriptionCreated(context.Background(), writer, audit.SubscriptionCreated{
		SubscriptionID:     subID,
		TenantID:           tenantID,
		PlanID:             planID,
		CurrentPeriodStart: periodStart,
		ActorUserID:        actorID,
	})
	if err != nil {
		t.Fatalf("WriteSubscriptionCreated unexpected error: %v", err)
	}
	if len(writer.security) != 1 {
		t.Fatalf("expected 1 row, got %d", len(writer.security))
	}
	ev := writer.security[0]
	if ev.Event != audit.SecurityEventSubscriptionCreated {
		t.Fatalf("event=%q, want %q", ev.Event, audit.SecurityEventSubscriptionCreated)
	}
	if ev.Target["outcome"] != "allow" {
		t.Fatalf("target.outcome defaulted incorrectly: %v", ev.Target["outcome"])
	}
	if ev.Target["subscription_id"] != subID.String() {
		t.Fatalf("target.subscription_id=%v, want %q", ev.Target["subscription_id"], subID)
	}
	if ev.Target["plan_id"] != planID.String() {
		t.Fatalf("target.plan_id=%v, want %q", ev.Target["plan_id"], planID)
	}
	if ev.Target["current_period_start"] != periodStart.Format(time.RFC3339Nano) {
		t.Fatalf("target.current_period_start=%v, want %q", ev.Target["current_period_start"], periodStart)
	}
}

func TestWriteSubscriptionCreated_RejectsZeroFields(t *testing.T) {
	t.Parallel()
	base := audit.SubscriptionCreated{
		SubscriptionID:     uuid.New(),
		TenantID:           uuid.New(),
		PlanID:             uuid.New(),
		CurrentPeriodStart: time.Now(),
		ActorUserID:        uuid.New(),
	}
	writer := &recordingSplitLogger{}
	tests := []struct {
		name string
		mut  func(*audit.SubscriptionCreated)
	}{
		{"zero actor", func(e *audit.SubscriptionCreated) { e.ActorUserID = uuid.Nil }},
		{"zero tenant", func(e *audit.SubscriptionCreated) { e.TenantID = uuid.Nil }},
		{"zero subscription", func(e *audit.SubscriptionCreated) { e.SubscriptionID = uuid.Nil }},
		{"zero plan", func(e *audit.SubscriptionCreated) { e.PlanID = uuid.Nil }},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := base
			tc.mut(&ev)
			err := audit.WriteSubscriptionCreated(context.Background(), writer, ev)
			if !errors.Is(err, audit.ErrInvalidBillingAuditEvent) {
				t.Fatalf("expected ErrInvalidBillingAuditEvent, got %v", err)
			}
		})
	}
}

func TestWriteInvoiceCancelledByMaster_AllowPath(t *testing.T) {
	t.Parallel()
	writer := &recordingSplitLogger{}
	invID := uuid.New()
	tenantID := uuid.New()
	actorID := uuid.New()
	periodStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	err := audit.WriteInvoiceCancelledByMaster(context.Background(), writer, audit.InvoiceCancelledByMaster{
		InvoiceID:   invID,
		TenantID:    tenantID,
		PeriodStart: periodStart,
		Reason:      "duplicate invoice from gateway",
		ActorUserID: actorID,
		Outcome:     audit.OutcomeAllow,
	})
	if err != nil {
		t.Fatalf("WriteInvoiceCancelledByMaster unexpected error: %v", err)
	}
	if len(writer.security) != 1 {
		t.Fatalf("expected 1 row, got %d", len(writer.security))
	}
	ev := writer.security[0]
	if ev.Event != audit.SecurityEventInvoiceCancelledByMaster {
		t.Fatalf("event=%q, want %q", ev.Event, audit.SecurityEventInvoiceCancelledByMaster)
	}
	if ev.Target["invoice_id"] != invID.String() {
		t.Fatalf("target.invoice_id=%v, want %q", ev.Target["invoice_id"], invID)
	}
	if ev.Target["reason"] != "duplicate invoice from gateway" {
		t.Fatalf("target.reason=%v", ev.Target["reason"])
	}
	if ev.Target["outcome"] != "allow" {
		t.Fatalf("target.outcome=%v, want allow", ev.Target["outcome"])
	}
}

func TestWriteInvoiceCancelledByMaster_DenyPath(t *testing.T) {
	t.Parallel()
	writer := &recordingSplitLogger{}

	err := audit.WriteInvoiceCancelledByMaster(context.Background(), writer, audit.InvoiceCancelledByMaster{
		InvoiceID:   uuid.New(),
		TenantID:    uuid.New(),
		PeriodStart: time.Now(),
		Reason:      "deny path: rls rejected the write",
		ActorUserID: uuid.New(),
		Outcome:     audit.OutcomeDeny,
	})
	if err != nil {
		t.Fatalf("WriteInvoiceCancelledByMaster unexpected error: %v", err)
	}
	if writer.security[0].Target["outcome"] != "deny" {
		t.Fatalf("target.outcome=%v, want deny", writer.security[0].Target["outcome"])
	}
}

func TestWriteInvoiceCancelledByMaster_RejectsZeroFields(t *testing.T) {
	t.Parallel()
	base := audit.InvoiceCancelledByMaster{
		InvoiceID:   uuid.New(),
		TenantID:    uuid.New(),
		PeriodStart: time.Now(),
		Reason:      "valid reason >=10 chars",
		ActorUserID: uuid.New(),
	}
	writer := &recordingSplitLogger{}
	tests := []struct {
		name string
		mut  func(*audit.InvoiceCancelledByMaster)
	}{
		{"zero actor", func(e *audit.InvoiceCancelledByMaster) { e.ActorUserID = uuid.Nil }},
		{"zero tenant", func(e *audit.InvoiceCancelledByMaster) { e.TenantID = uuid.Nil }},
		{"zero invoice", func(e *audit.InvoiceCancelledByMaster) { e.InvoiceID = uuid.Nil }},
		{"empty reason", func(e *audit.InvoiceCancelledByMaster) { e.Reason = "" }},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := base
			tc.mut(&ev)
			err := audit.WriteInvoiceCancelledByMaster(context.Background(), writer, ev)
			if !errors.Is(err, audit.ErrInvalidBillingAuditEvent) {
				t.Fatalf("expected ErrInvalidBillingAuditEvent, got %v", err)
			}
		})
	}
}

func TestWriteBilling_NilWriter(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	actorID := uuid.New()
	cases := []struct {
		name string
		call func() error
	}{
		{"grant", func() error {
			return audit.WriteMasterGrantIssued(context.Background(), nil, audit.MasterGrantIssued{
				GrantID: uuid.New(), Kind: "extra_tokens", TenantID: tenantID, ActorUserID: actorID, Reason: "rejected nil writer",
			})
		}},
		{"subscription", func() error {
			return audit.WriteSubscriptionCreated(context.Background(), nil, audit.SubscriptionCreated{
				SubscriptionID: uuid.New(), TenantID: tenantID, PlanID: uuid.New(), ActorUserID: actorID,
			})
		}},
		{"invoice", func() error {
			return audit.WriteInvoiceCancelledByMaster(context.Background(), nil, audit.InvoiceCancelledByMaster{
				InvoiceID: uuid.New(), TenantID: tenantID, ActorUserID: actorID, Reason: "rejected nil writer",
			})
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.call(); !errors.Is(err, audit.ErrInvalidBillingAuditEvent) {
				t.Fatalf("expected ErrInvalidBillingAuditEvent, got %v", err)
			}
		})
	}
}

func TestWriteBilling_WriterErrorPropagates(t *testing.T) {
	t.Parallel()
	boom := errors.New("postgres: insert audit_log_security: connection refused")
	writer := &recordingSplitLogger{failWith: boom}

	err := audit.WriteMasterGrantIssued(context.Background(), writer, audit.MasterGrantIssued{
		GrantID: uuid.New(), Kind: "extra_tokens", TenantID: uuid.New(), ActorUserID: uuid.New(),
		Reason: "writer error path",
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected writer error to propagate, got %v", err)
	}
}
