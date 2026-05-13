package iam

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNewSessionID_VersionAndUniqueness(t *testing.T) {
	seen := make(map[uuid.UUID]struct{}, 100)
	for i := 0; i < 100; i++ {
		id, err := NewSessionID()
		if err != nil {
			t.Fatalf("NewSessionID: %v", err)
		}
		if id == uuid.Nil {
			t.Fatalf("NewSessionID returned uuid.Nil")
		}
		if id.Version() != 4 {
			t.Fatalf("session id version %d, want v4", id.Version())
		}
		if id.Variant() != uuid.RFC4122 {
			t.Fatalf("session id variant %v, want RFC4122", id.Variant())
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("collision in 100 NewSessionID calls — entropy broken")
		}
		seen[id] = struct{}{}
	}
}

func TestSession_IsExpired(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s := Session{ExpiresAt: t0}

	if s.IsExpired(t0.Add(-time.Second)) {
		t.Fatalf("IsExpired before ExpiresAt should be false")
	}
	if !s.IsExpired(t0) {
		t.Fatalf("IsExpired exactly at ExpiresAt should be true (boundary inclusive)")
	}
	if !s.IsExpired(t0.Add(time.Second)) {
		t.Fatalf("IsExpired after ExpiresAt should be true")
	}
}

func TestParseSessionTTL(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		want    time.Duration
		wantErr bool
	}{
		{"empty-default", "", 24 * time.Hour, false},
		{"valid-1m", "1m", time.Minute, false},
		{"valid-12h", "12h", 12 * time.Hour, false},
		{"valid-30d", "720h", 30 * 24 * time.Hour, false},
		{"too-small-30s", "30s", 0, true},
		{"too-small-default-zero-ns", "0s", 0, true},
		{"too-large", "31d", 0, true},              // ParseDuration won't accept "31d" — error path
		{"unparseable", "not-a-duration", 0, true}, // ParseDuration error
		{"missing-unit-foot-gun", "24", 0, true},   // bare integer parsed as 24ns; rejected by bounds
		{"too-large-real-units", "721h", 0, true},  // > 30 days
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			got, err := ParseSessionTTL(func(string) string { return env })
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil; got=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestParseSessionTTL_NilGetenv(t *testing.T) {
	got, err := ParseSessionTTL(nil)
	if err != nil {
		t.Fatalf("nil getenv should fall back to default, got error: %v", err)
	}
	if got != 24*time.Hour {
		t.Fatalf("nil getenv default: got %v want 24h", got)
	}
}

func TestMustParseSessionTTL_FatalOnInvalid(t *testing.T) {
	called := false
	got := MustParseSessionTTL(
		func(string) string { return "bogus" },
		func(format string, args ...any) { called = true },
	)
	if !called {
		t.Fatalf("fatal callback not invoked on invalid TTL")
	}
	if got != 0 {
		t.Fatalf("MustParseSessionTTL after fatal should return zero, got %v", got)
	}
}

func TestMustParseSessionTTL_ValidPath(t *testing.T) {
	got := MustParseSessionTTL(func(string) string { return "2h" }, nil)
	if got != 2*time.Hour {
		t.Fatalf("got %v want 2h", got)
	}
}
