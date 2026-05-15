package authz_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/authz"
)

// fakeSplitLogger records WriteSecurity invocations so tests can
// assert payload shape and order. Concurrency-safe.
type fakeSplitLogger struct {
	mu     sync.Mutex
	events []audit.SecurityAuditEvent
	err    error
}

func (f *fakeSplitLogger) WriteSecurity(_ context.Context, ev audit.SecurityAuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return f.err
}

func (f *fakeSplitLogger) WriteData(context.Context, audit.DataAuditEvent) error {
	return nil
}

func (f *fakeSplitLogger) snapshot() []audit.SecurityAuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]audit.SecurityAuditEvent, len(f.events))
	copy(out, f.events)
	return out
}

func newRecorderFixture() (*authz.AuditRecorder, *fakeSplitLogger, *authz.Metrics, *bytes.Buffer) {
	w := &fakeSplitLogger{}
	m := authz.NewMetrics(nil)
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return authz.NewAuditRecorder(w, m, log), w, m, logBuf
}

func TestAuditRecorder_Record_DenyWritesAuthzDenyEvent(t *testing.T) {
	t.Parallel()
	r, w, m, _ := newRecorderFixture()
	p := iam.Principal{
		UserID:   uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		TenantID: uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		Roles:    []iam.Role{iam.RoleTenantAtendente},
	}
	d := iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC, TargetKind: "conversation", TargetID: "c-1"}
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

	r.Record(context.Background(), p, iam.ActionTenantConversationRead, iam.Resource{Kind: "conversation", ID: "c-1"}, d, now)

	events := w.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Event != audit.SecurityEventAuthzDeny {
		t.Fatalf("event_type = %s, want authz_deny", ev.Event)
	}
	if ev.ActorUserID != p.UserID {
		t.Fatalf("actor_user_id = %v, want %v", ev.ActorUserID, p.UserID)
	}
	if ev.TenantID == nil || *ev.TenantID != p.TenantID {
		t.Fatalf("tenant_id mismatch: %v", ev.TenantID)
	}
	if !ev.OccurredAt.Equal(now) {
		t.Fatalf("occurred_at = %v, want %v", ev.OccurredAt, now)
	}
	wantTarget := map[string]any{
		"outcome":     "deny",
		"action":      "tenant.conversation.read",
		"reason_code": "denied_rbac",
		"target_kind": "conversation",
		"target_id":   "c-1",
	}
	for k, v := range wantTarget {
		if ev.Target[k] != v {
			t.Fatalf("target[%q] = %v, want %v", k, ev.Target[k], v)
		}
	}
	if got := testutil.ToFloat64(m.UserDeny.WithLabelValues(p.UserID.String(), p.TenantID.String())); got != 1 {
		t.Fatalf("user_deny counter = %v, want 1", got)
	}
}

func TestAuditRecorder_Record_AllowWritesAuthzAllowEvent(t *testing.T) {
	t.Parallel()
	r, w, m, _ := newRecorderFixture()
	p := iam.Principal{
		UserID:   uuid.MustParse("00000000-0000-0000-0000-000000000003"),
		TenantID: uuid.MustParse("00000000-0000-0000-0000-000000000004"),
		Roles:    []iam.Role{iam.RoleTenantAtendente},
	}
	d := iam.Decision{Allow: true, ReasonCode: iam.ReasonAllowedRBAC, TargetKind: "contact", TargetID: "k-9"}
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

	r.Record(context.Background(), p, iam.ActionTenantContactRead, iam.Resource{Kind: "contact", ID: "k-9"}, d, now)

	events := w.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Event != audit.SecurityEventAuthzAllow {
		t.Fatalf("event_type = %s, want authz_allow", events[0].Event)
	}
	if events[0].Target["outcome"] != "allow" {
		t.Fatalf("target.outcome = %v, want allow", events[0].Target["outcome"])
	}
	// Allow path does NOT increment user_deny.
	if got := testutil.ToFloat64(m.UserDeny.WithLabelValues(p.UserID.String(), p.TenantID.String())); got != 0 {
		t.Fatalf("user_deny incremented on allow: %v", got)
	}
}

func TestAuditRecorder_Record_NilTenantWhenPrincipalTenantNil(t *testing.T) {
	t.Parallel()
	r, w, _, _ := newRecorderFixture()
	p := iam.Principal{
		UserID:   uuid.MustParse("00000000-0000-0000-0000-000000000005"),
		TenantID: uuid.Nil,
		Roles:    []iam.Role{iam.RoleMaster},
	}
	d := iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC}
	r.Record(context.Background(), p, iam.ActionMasterTenantRead, iam.Resource{}, d, time.Now())
	events := w.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].TenantID != nil {
		t.Fatalf("tenant_id should be nil for tenant-less principal, got %v", events[0].TenantID)
	}
}

func TestAuditRecorder_Record_SkipsAuditWriteForNilActor(t *testing.T) {
	t.Parallel()
	r, w, m, logBuf := newRecorderFixture()
	p := iam.Principal{UserID: uuid.Nil, TenantID: uuid.MustParse("00000000-0000-0000-0000-000000000007")}
	d := iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedNoPrincipal}
	r.Record(context.Background(), p, iam.ActionTenantContactRead, iam.Resource{}, d, time.Now())
	if len(w.snapshot()) != 0 {
		t.Fatalf("write should be skipped for nil actor")
	}
	if !strings.Contains(logBuf.String(), "authz_audit_skipped_no_actor") {
		t.Fatalf("expected warn log for nil actor, got %s", logBuf.String())
	}
	// Metric still increments so a nil-actor probing burst is visible.
	if got := testutil.ToFloat64(m.UserDeny.WithLabelValues(p.UserID.String(), p.TenantID.String())); got != 1 {
		t.Fatalf("user_deny counter = %v, want 1", got)
	}
}

func TestAuditRecorder_Record_WriteFailureIsLoggedNotPropagated(t *testing.T) {
	t.Parallel()
	r, w, _, logBuf := newRecorderFixture()
	w.err = errors.New("postgres down")
	p := iam.Principal{
		UserID:   uuid.MustParse("00000000-0000-0000-0000-000000000008"),
		TenantID: uuid.MustParse("00000000-0000-0000-0000-000000000009"),
	}
	r.Record(context.Background(), p, iam.ActionTenantContactRead, iam.Resource{},
		iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC}, time.Now())
	if !strings.Contains(logBuf.String(), "authz_audit_write_failed") {
		t.Fatalf("expected warn log for write failure, got %s", logBuf.String())
	}
}

func TestNewAuditRecorder_PanicsOnNilWriter(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = authz.NewAuditRecorder(nil, authz.NewMetrics(nil), nil)
}

func TestNewAuditRecorder_PanicsOnNilMetrics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = authz.NewAuditRecorder(&fakeSplitLogger{}, nil, nil)
}

func TestNewAuditRecorder_NilLogDefaults(t *testing.T) {
	t.Parallel()
	rec := authz.NewAuditRecorder(&fakeSplitLogger{}, authz.NewMetrics(nil), nil)
	// Smoke: invoke Record on a happy-path event; nil-log fallback to
	// slog.Default must not panic.
	p := iam.Principal{
		UserID:   uuid.MustParse("00000000-0000-0000-0000-000000000010"),
		TenantID: uuid.MustParse("00000000-0000-0000-0000-000000000011"),
	}
	rec.Record(context.Background(), p, iam.ActionTenantContactRead, iam.Resource{},
		iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC}, time.Now())
}
