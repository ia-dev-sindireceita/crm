package slog

// SIN-62418 — slog adapter coverage for LogHardCapHit, the master.
// session.hard_cap_hit event added so dashboards can split breach
// attempts (master operator riding past created_at + 4h) from the
// benign idle-timeout churn that already lands in master_mfa_required
// or the implicit "session vanished" redirect.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLogHardCapHit_IncludesAllAuditFields(t *testing.T) {
	a, buf := newCapturingAudit(t)
	userID := uuid.New()
	sessionID := uuid.New()
	createdAt := time.Date(2026, 5, 9, 10, 0, 0, 123456789, time.UTC)
	now := createdAt.Add(4*time.Hour + time.Minute)
	route := "/m/2fa/enroll"

	if err := a.LogHardCapHit(context.Background(), userID, sessionID, createdAt, now, route); err != nil {
		t.Fatalf("LogHardCapHit: %v", err)
	}

	rec := decodeFirstRecord(t, buf)
	if rec["event"] != EventSessionHardCapHit {
		t.Errorf("event: got %q want %q", rec["event"], EventSessionHardCapHit)
	}
	if rec["user_id"] != userID.String() {
		t.Errorf("user_id: got %q want %q", rec["user_id"], userID.String())
	}
	if rec["session_id"] != sessionID.String() {
		t.Errorf("session_id: got %q want %q", rec["session_id"], sessionID.String())
	}
	if rec["route"] != route {
		t.Errorf("route: got %q want %q", rec["route"], route)
	}

	// Timestamps are emitted as RFC3339Nano so dashboards can compute
	// the breach offset (now - created_at) directly. Decode and compare
	// instants rather than literal strings — Go drops trailing zero
	// nanos which would otherwise force a brittle string match.
	gotCreated, err := time.Parse(time.RFC3339Nano, rec["created_at"].(string))
	if err != nil {
		t.Fatalf("parse created_at %q: %v", rec["created_at"], err)
	}
	if !gotCreated.Equal(createdAt) {
		t.Errorf("created_at: got %v want %v", gotCreated, createdAt)
	}
	gotNow, err := time.Parse(time.RFC3339Nano, rec["now"].(string))
	if err != nil {
		t.Fatalf("parse now %q: %v", rec["now"], err)
	}
	if !gotNow.Equal(now) {
		t.Errorf("now: got %v want %v", gotNow, now)
	}
	if rec["level"] != "INFO" {
		t.Errorf("level: got %q want INFO", rec["level"])
	}
}

// EventSessionHardCapHit is grep'd by SIEM dashboards on the literal
// string "master.session.hard_cap_hit"; pin it so a future refactor
// trips the build before it reaches production.
func TestEventSessionHardCapHit_IsPinnedToADRString(t *testing.T) {
	t.Parallel()
	if EventSessionHardCapHit != "master.session.hard_cap_hit" {
		t.Errorf("EventSessionHardCapHit = %q, want %q", EventSessionHardCapHit, "master.session.hard_cap_hit")
	}
}

// LogHardCapHit normalises non-UTC inputs to UTC so dashboards never
// have to handle offset-shifted timestamps. The test feeds a
// non-UTC time and asserts the emitted RFC3339Nano string ends in "Z".
func TestLogHardCapHit_NormalisesTimestampsToUTC(t *testing.T) {
	a, buf := newCapturingAudit(t)
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("tzdata missing: %v", err)
	}
	createdAt := time.Date(2026, 5, 9, 7, 0, 0, 0, loc) // -03:00
	now := time.Date(2026, 5, 9, 11, 1, 0, 0, loc)      // -03:00
	if err := a.LogHardCapHit(context.Background(), uuid.New(), uuid.New(), createdAt, now, "/m/x"); err != nil {
		t.Fatalf("LogHardCapHit: %v", err)
	}
	rec := decodeFirstRecord(t, buf)
	for _, key := range []string{"created_at", "now"} {
		s, ok := rec[key].(string)
		if !ok {
			t.Fatalf("%s missing or not string: %v", key, rec[key])
		}
		if s[len(s)-1] != 'Z' {
			t.Errorf("%s = %q, expected trailing Z (UTC normalisation)", key, s)
		}
	}
}
