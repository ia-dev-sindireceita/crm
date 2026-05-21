package lgpd_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/lgpd"
)

func TestDeletionStatus_Valid(t *testing.T) {
	for _, s := range []lgpd.DeletionStatus{
		lgpd.DeletionStatusPending,
		lgpd.DeletionStatusCompleted,
		lgpd.DeletionStatusFailed,
	} {
		if !s.Valid() {
			t.Errorf("Valid(%q) = false, want true", s)
		}
	}
	for _, s := range []lgpd.DeletionStatus{"", "deleted", "in_progress"} {
		if s.Valid() {
			t.Errorf("Valid(%q) = true, want false", s)
		}
	}
}

func TestDeletionRequest_Validate(t *testing.T) {
	base := lgpd.DeletionRequest{
		TenantID:       uuid.New(),
		ContactID:      uuid.New(),
		Justification:  "test",
		Status:         lgpd.DeletionStatusPending,
		RetentionUntil: time.Now().Add(time.Hour),
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("Validate(base) err = %v, want nil", err)
	}

	tests := []struct {
		name   string
		mutate func(*lgpd.DeletionRequest)
		want   string
	}{
		{"no tenant", func(r *lgpd.DeletionRequest) { r.TenantID = uuid.Nil }, "tenant_id"},
		{"no contact", func(r *lgpd.DeletionRequest) { r.ContactID = uuid.Nil }, "contact_id"},
		{"no justification", func(r *lgpd.DeletionRequest) { r.Justification = "" }, "justification"},
		{"bad status", func(r *lgpd.DeletionRequest) { r.Status = "weird" }, "status"},
		{"no retention", func(r *lgpd.DeletionRequest) { r.RetentionUntil = time.Time{} }, "retention_until"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("Validate(%s) err = nil, want non-nil", tc.name)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("Validate(%s) err = %v; want it to mention %q", tc.name, err, tc.want)
			}
		})
	}
}

func TestRetentionPolicy_Defaults(t *testing.T) {
	p, err := lgpd.NewRetentionPolicy(0)
	if err != nil {
		t.Fatalf("NewRetentionPolicy(0) err = %v", err)
	}
	if p.FiscalYears != lgpd.DefaultFiscalRetentionYears {
		t.Errorf("default FiscalYears = %d, want %d", p.FiscalYears, lgpd.DefaultFiscalRetentionYears)
	}
}

func TestRetentionPolicy_NegativeRejected(t *testing.T) {
	if _, err := lgpd.NewRetentionPolicy(-1); err == nil {
		t.Fatal("NewRetentionPolicy(-1) err = nil, want non-nil")
	}
}

func TestRetentionPolicy_RetentionUntil(t *testing.T) {
	p, _ := lgpd.NewRetentionPolicy(5)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	got := p.RetentionUntil(now)
	want := time.Date(2031, 1, 15, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("RetentionUntil(%v) = %v, want %v", now, got, want)
	}
}

func TestErrDeletionRequestNotFound(t *testing.T) {
	if !errors.Is(lgpd.ErrDeletionRequestNotFound, lgpd.ErrDeletionRequestNotFound) {
		t.Fatal("errors.Is(ErrDeletionRequestNotFound, itself) = false")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
