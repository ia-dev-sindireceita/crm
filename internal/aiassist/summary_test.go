package aiassist_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aiassist"
)

func TestNewSummary_ValidationErrors(t *testing.T) {
	t.Parallel()

	tenant := uuid.New()
	conv := uuid.New()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		tenant  uuid.UUID
		conv    uuid.UUID
		text    string
		model   string
		in, out int64
		want    error
	}{
		{
			name:   "zero tenant",
			tenant: uuid.Nil,
			conv:   conv,
			text:   "ok",
			model:  "m",
			want:   aiassist.ErrZeroTenant,
		},
		{
			name:   "zero conversation",
			tenant: tenant,
			conv:   uuid.Nil,
			text:   "ok",
			model:  "m",
			want:   aiassist.ErrZeroConversation,
		},
		{
			name:   "empty text",
			tenant: tenant,
			conv:   conv,
			text:   "",
			model:  "m",
			want:   aiassist.ErrEmptyPrompt,
		},
		{
			name:   "empty model",
			tenant: tenant,
			conv:   conv,
			text:   "ok",
			model:  "",
			want:   aiassist.ErrEmptyPrompt,
		},
		{
			name:   "negative tokens",
			tenant: tenant,
			conv:   conv,
			text:   "ok",
			model:  "m",
			in:     -1,
			want:   aiassist.ErrEmptyPrompt,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := aiassist.NewSummary(tc.tenant, tc.conv, tc.text, tc.model, tc.in, tc.out, now, aiassist.DefaultSummaryTTL)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewSummary_AppliesTTL(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conv := uuid.New()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	s, err := aiassist.NewSummary(tenant, conv, "snippet", "openai/gpt-4o", 100, 50, now, 2*time.Hour)
	if err != nil {
		t.Fatalf("NewSummary: %v", err)
	}
	want := now.Add(2 * time.Hour)
	if !s.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", s.ExpiresAt, want)
	}
	if s.GeneratedAt != now {
		t.Fatalf("GeneratedAt mismatch")
	}
	if s.TokensIn != 100 || s.TokensOut != 50 {
		t.Fatalf("token fields not persisted")
	}
}

func TestNewSummary_ZeroTTLLeavesExpiresZero(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conv := uuid.New()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	s, err := aiassist.NewSummary(tenant, conv, "snippet", "m", 1, 1, now, 0)
	if err != nil {
		t.Fatalf("NewSummary: %v", err)
	}
	if !s.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt = %v, want zero", s.ExpiresAt)
	}
}

func TestSummary_IsValid(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conv := uuid.New()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		mutate  func(*aiassist.Summary)
		check   time.Time
		want    bool
		stepDoc string
	}{
		{
			name:   "fresh inside TTL",
			mutate: func(s *aiassist.Summary) {},
			check:  now.Add(1 * time.Hour),
			want:   true,
		},
		{
			name:   "expired right after TTL",
			mutate: func(s *aiassist.Summary) {},
			check:  now.Add(24*time.Hour + time.Second),
			want:   false,
		},
		{
			name:   "at exact TTL boundary still valid",
			mutate: func(s *aiassist.Summary) {},
			check:  now.Add(24 * time.Hour),
			want:   true,
		},
		{
			name: "explicitly invalidated",
			mutate: func(s *aiassist.Summary) {
				s.Invalidate(now.Add(30 * time.Minute))
			},
			check: now.Add(1 * time.Hour),
			want:  false,
		},
		{
			name: "no TTL stays valid forever",
			mutate: func(s *aiassist.Summary) {
				s.ExpiresAt = time.Time{}
			},
			check: now.Add(72 * time.Hour),
			want:  true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := aiassist.NewSummary(tenant, conv, "snippet", "m", 1, 1, now, aiassist.DefaultSummaryTTL)
			if err != nil {
				t.Fatalf("NewSummary: %v", err)
			}
			tc.mutate(s)
			got := s.IsValid(tc.check)
			if got != tc.want {
				t.Fatalf("IsValid(%v) = %v, want %v", tc.check, got, tc.want)
			}
		})
	}
}

func TestSummary_InvalidateIdempotent(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conv := uuid.New()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	s, err := aiassist.NewSummary(tenant, conv, "snippet", "m", 1, 1, now, aiassist.DefaultSummaryTTL)
	if err != nil {
		t.Fatalf("NewSummary: %v", err)
	}
	first := now.Add(1 * time.Hour)
	second := now.Add(2 * time.Hour)
	s.Invalidate(first)
	s.Invalidate(second)
	if !s.InvalidatedAt.Equal(first) {
		t.Fatalf("InvalidatedAt = %v, want first invalidation %v", s.InvalidatedAt, first)
	}
}

func TestSummary_IsValidNilReceiver(t *testing.T) {
	t.Parallel()
	var s *aiassist.Summary
	if s.IsValid(time.Now()) {
		t.Fatalf("nil receiver IsValid must be false")
	}
}

func TestSummary_InvalidateNilReceiver(t *testing.T) {
	t.Parallel()
	var s *aiassist.Summary
	s.Invalidate(time.Now()) // must not panic
}
