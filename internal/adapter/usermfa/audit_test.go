package usermfa

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

func TestNewTenantAuditLoggerValidates(t *testing.T) {
	t.Parallel()
	if _, err := NewTenantAuditLogger(nil, uuid.New()); err == nil {
		t.Fatalf("expected error for nil writer")
	}
	if _, err := NewTenantAuditLogger(&fakeWriter{}, uuid.Nil); err == nil {
		t.Fatalf("expected error for zero tenant id")
	}
}

func TestTenantAuditLoggerWritesAllEventTypes(t *testing.T) {
	t.Parallel()
	w := &fakeWriter{}
	tenantID := uuid.New()
	l, err := NewTenantAuditLogger(w, tenantID)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	userID := uuid.New()
	ctx := context.Background()

	if err := l.LogEnrolled(ctx, userID); err != nil {
		t.Fatalf("LogEnrolled: %v", err)
	}
	if err := l.LogVerified(ctx, userID); err != nil {
		t.Fatalf("LogVerified: %v", err)
	}
	if err := l.LogRecoveryUsed(ctx, userID); err != nil {
		t.Fatalf("LogRecoveryUsed: %v", err)
	}
	if err := l.LogRecoveryRegenerated(ctx, userID); err != nil {
		t.Fatalf("LogRecoveryRegenerated: %v", err)
	}
	if err := l.LogMFARequired(ctx, userID, "/admin/x", "missing_pending"); err != nil {
		t.Fatalf("LogMFARequired: %v", err)
	}

	wanted := []audit.SecurityEvent{
		audit.SecurityEvent2FAEnroll,
		audit.SecurityEvent2FAVerify,
		audit.SecurityEvent2FARecoveryUsed,
		audit.SecurityEvent2FARecoveryRegenerated,
		audit.SecurityEvent2FARequired,
	}
	if got := len(w.events()); got != len(wanted) {
		t.Fatalf("expected %d events, got %d", len(wanted), got)
	}
	for i, want := range wanted {
		ev := w.events()[i]
		if ev.Event != want {
			t.Errorf("event %d: want %s got %s", i, want, ev.Event)
		}
		if ev.ActorUserID != userID {
			t.Errorf("event %d: want actor %s got %s", i, userID, ev.ActorUserID)
		}
		if ev.TenantID == nil || *ev.TenantID != tenantID {
			t.Errorf("event %d: want tenant %s got %v", i, tenantID, ev.TenantID)
		}
	}
	last := w.events()[len(w.events())-1]
	if last.Target["route"] != "/admin/x" {
		t.Errorf("last target.route: want /admin/x got %v", last.Target["route"])
	}
	if last.Target["reason"] != "missing_pending" {
		t.Errorf("last target.reason: want missing_pending got %v", last.Target["reason"])
	}
}

func TestTenantAuditLoggerPropagatesWriterError(t *testing.T) {
	t.Parallel()
	w := &fakeWriter{err: errors.New("boom")}
	l, err := NewTenantAuditLogger(w, uuid.New())
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if err := l.LogEnrolled(context.Background(), uuid.New()); err == nil {
		t.Fatalf("expected error to propagate")
	}
}

// fakeWriter satisfies audit.SplitLogger.
type fakeWriter struct {
	mu   sync.Mutex
	got  []audit.SecurityAuditEvent
	dgot []audit.DataAuditEvent
	err  error
}

func (f *fakeWriter) WriteSecurity(_ context.Context, e audit.SecurityAuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.got = append(f.got, e)
	return nil
}

func (f *fakeWriter) WriteData(_ context.Context, e audit.DataAuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.dgot = append(f.dgot, e)
	return nil
}

func (f *fakeWriter) events() []audit.SecurityAuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]audit.SecurityAuditEvent, len(f.got))
	copy(out, f.got)
	return out
}
