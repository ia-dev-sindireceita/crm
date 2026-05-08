package slog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// newCapturingAudit returns an MFAAudit whose underlying logger writes
// to a *bytes.Buffer in JSON form. Tests decode the buffer into a
// map[string]any to assert on stable attribute names without coupling
// to slog's internal record layout.
func newCapturingAudit(t *testing.T) (*MFAAudit, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	a, err := NewMFAAudit(slog.New(h))
	if err != nil {
		t.Fatalf("NewMFAAudit: %v", err)
	}
	return a, buf
}

func TestNewMFAAudit_RejectsNilLogger(t *testing.T) {
	if _, err := NewMFAAudit(nil); err == nil {
		t.Fatal("NewMFAAudit(nil) returned no error")
	}
}

func decodeFirstRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(strings.SplitN(buf.String(), "\n", 2)[0])
	if line == "" {
		t.Fatalf("logger emitted nothing")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("decode: %v (line=%q)", err, line)
	}
	return rec
}

func assertEvent(t *testing.T, rec map[string]any, wantEvent, wantUser string) {
	t.Helper()
	if rec["event"] != wantEvent {
		t.Errorf("event: got %q want %q", rec["event"], wantEvent)
	}
	if rec["msg"] != wantEvent {
		t.Errorf("msg: got %q want %q (msg should mirror event for grep convenience)", rec["msg"], wantEvent)
	}
	if rec["user_id"] != wantUser {
		t.Errorf("user_id: got %q want %q", rec["user_id"], wantUser)
	}
	if rec["level"] != "INFO" {
		t.Errorf("level: got %q want INFO", rec["level"])
	}
}

func TestLogEnrolled(t *testing.T) {
	a, buf := newCapturingAudit(t)
	uid := uuid.New()
	if err := a.LogEnrolled(context.Background(), uid); err != nil {
		t.Fatalf("LogEnrolled: %v", err)
	}
	assertEvent(t, decodeFirstRecord(t, buf), EventEnrolled, uid.String())
}

func TestLogVerified(t *testing.T) {
	a, buf := newCapturingAudit(t)
	uid := uuid.New()
	if err := a.LogVerified(context.Background(), uid); err != nil {
		t.Fatalf("LogVerified: %v", err)
	}
	assertEvent(t, decodeFirstRecord(t, buf), EventVerified, uid.String())
}

func TestLogRecoveryUsed(t *testing.T) {
	a, buf := newCapturingAudit(t)
	uid := uuid.New()
	if err := a.LogRecoveryUsed(context.Background(), uid); err != nil {
		t.Fatalf("LogRecoveryUsed: %v", err)
	}
	assertEvent(t, decodeFirstRecord(t, buf), EventRecoveryUsed, uid.String())
}

func TestLogRecoveryRegenerated(t *testing.T) {
	a, buf := newCapturingAudit(t)
	uid := uuid.New()
	if err := a.LogRecoveryRegenerated(context.Background(), uid); err != nil {
		t.Fatalf("LogRecoveryRegenerated: %v", err)
	}
	assertEvent(t, decodeFirstRecord(t, buf), EventRecoveryRegenerated, uid.String())
}

func TestLogMFARequired_IncludesRouteAndReason(t *testing.T) {
	a, buf := newCapturingAudit(t)
	uid := uuid.New()
	if err := a.LogMFARequired(context.Background(), uid, "/m/tenant", "not_verified"); err != nil {
		t.Fatalf("LogMFARequired: %v", err)
	}
	rec := decodeFirstRecord(t, buf)
	assertEvent(t, rec, EventMFARequired, uid.String())
	if rec["route"] != "/m/tenant" {
		t.Errorf("route: got %q want /m/tenant", rec["route"])
	}
	if rec["reason"] != "not_verified" {
		t.Errorf("reason: got %q want not_verified", rec["reason"])
	}
}

func TestEventNamesArePinned(t *testing.T) {
	// ADR 0074 dashboards filter on these literal strings. If a refactor
	// silently renames one of them, this assertion fails before it
	// reaches production.
	cases := map[string]string{
		"EventEnrolled":            EventEnrolled,
		"EventVerified":            EventVerified,
		"EventRecoveryUsed":        EventRecoveryUsed,
		"EventRecoveryRegenerated": EventRecoveryRegenerated,
		"EventMFARequired":         EventMFARequired,
	}
	want := map[string]string{
		"EventEnrolled":            "master_mfa_enrolled",
		"EventVerified":            "master_mfa_verified",
		"EventRecoveryUsed":        "master_recovery_used",
		"EventRecoveryRegenerated": "master_recovery_regenerated",
		"EventMFARequired":         "master_mfa_required",
	}
	for name, got := range cases {
		if got != want[name] {
			t.Errorf("%s: got %q want %q", name, got, want[name])
		}
	}
}

// Sanity check that no method returns an error on nominal input — the
// port contract is "always nil unless logger is misconfigured" and
// callers MUST be able to ignore the return value.
func TestAllMethodsReturnNilOnHappyPath(t *testing.T) {
	a, _ := newCapturingAudit(t)
	uid := uuid.New()
	ctx := context.Background()
	calls := []func() error{
		func() error { return a.LogEnrolled(ctx, uid) },
		func() error { return a.LogVerified(ctx, uid) },
		func() error { return a.LogRecoveryUsed(ctx, uid) },
		func() error { return a.LogRecoveryRegenerated(ctx, uid) },
		func() error { return a.LogMFARequired(ctx, uid, "/r", "x") },
	}
	for i, c := range calls {
		if err := c(); err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	// Ensure the variable is referenced so go vet doesn't flag it.
	_ = errors.New
}
