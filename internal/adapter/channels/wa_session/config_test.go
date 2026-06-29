package wa_session

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		getenv  func(string) string
		wantMax int
	}{
		{"nil getenv → default", nil, defaultRateMaxPerMinute},
		{"empty → default", envFrom(nil), defaultRateMaxPerMinute},
		{"valid override", envFrom(map[string]string{EnvSessionRateMax: "120"}), 120},
		{"zero → default", envFrom(map[string]string{EnvSessionRateMax: "0"}), defaultRateMaxPerMinute},
		{"negative-ish non-numeric → default", envFrom(map[string]string{EnvSessionRateMax: "-5"}), defaultRateMaxPerMinute},
		{"garbage → default", envFrom(map[string]string{EnvSessionRateMax: "abc"}), defaultRateMaxPerMinute},
		{"overflow → default", envFrom(map[string]string{EnvSessionRateMax: "99999999"}), defaultRateMaxPerMinute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ConfigFromEnv(tt.getenv)
			if cfg.RateMaxPerMin != tt.wantMax {
				t.Errorf("RateMaxPerMin = %d, want %d", cfg.RateMaxPerMin, tt.wantMax)
			}
			if cfg.DeliverTimeout != defaultDeliverTimeout {
				t.Errorf("DeliverTimeout = %v, want %v", cfg.DeliverTimeout, defaultDeliverTimeout)
			}
		})
	}
}

func TestEnvFeatureFlag(t *testing.T) {
	tenant := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	other := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	tests := []struct {
		name   string
		getenv func(string) string
		id     uuid.UUID
		want   bool
	}{
		{"nil getenv → off", nil, tenant, false},
		{"global off → off", envFrom(map[string]string{EnvSessionEnabled: "0"}), tenant, false},
		{"global on, empty allowlist → on", envFrom(map[string]string{EnvSessionEnabled: "1"}), tenant, true},
		{
			name:   "global on, allowlisted → on",
			getenv: envFrom(map[string]string{EnvSessionEnabled: "1", EnvSessionTenantAllow: tenant.String()}),
			id:     tenant,
			want:   true,
		},
		{
			name:   "global on, not allowlisted → off",
			getenv: envFrom(map[string]string{EnvSessionEnabled: "1", EnvSessionTenantAllow: tenant.String()}),
			id:     other,
			want:   false,
		},
		{
			name:   "invalid uuid in allowlist dropped, valid kept",
			getenv: envFrom(map[string]string{EnvSessionEnabled: "1", EnvSessionTenantAllow: "not-a-uuid, " + tenant.String()}),
			id:     tenant,
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewEnvFeatureFlag(tt.getenv)
			got, err := f.Enabled(context.Background(), tt.id)
			if err != nil {
				t.Fatalf("Enabled: %v", err)
			}
			if got != tt.want {
				t.Errorf("Enabled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnvFeatureFlag_NilReceiver(t *testing.T) {
	var f *EnvFeatureFlag
	got, err := f.Enabled(context.Background(), uuid.New())
	if err != nil || got {
		t.Fatalf("nil flag Enabled = (%v,%v), want (false,nil)", got, err)
	}
}

func TestInMemoryRateLimiter(t *testing.T) {
	ctx := context.Background()

	t.Run("max<=0 denies", func(t *testing.T) {
		l := NewInMemoryRateLimiter()
		ok, _, err := l.Allow(ctx, "k", time.Minute, 0)
		if err != nil || ok {
			t.Fatalf("Allow = (%v,%v), want (false,nil)", ok, err)
		}
	})

	t.Run("allows up to max then denies in same window", func(t *testing.T) {
		now := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
		l := &InMemoryRateLimiter{now: func() time.Time { return now }, state: map[string]*window{}}
		for i := 0; i < 3; i++ {
			ok, _, _ := l.Allow(ctx, "k", time.Minute, 3)
			if !ok {
				t.Fatalf("hit %d denied, want allowed", i)
			}
		}
		ok, retry, _ := l.Allow(ctx, "k", time.Minute, 3)
		if ok {
			t.Fatal("4th hit allowed, want denied")
		}
		if retry <= 0 {
			t.Errorf("retryAfter = %v, want > 0", retry)
		}
	})

	t.Run("window resets", func(t *testing.T) {
		cur := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
		l := &InMemoryRateLimiter{now: func() time.Time { return cur }, state: map[string]*window{}}
		if ok, _, _ := l.Allow(ctx, "k", time.Minute, 1); !ok {
			t.Fatal("first hit denied")
		}
		if ok, _, _ := l.Allow(ctx, "k", time.Minute, 1); ok {
			t.Fatal("second hit in window allowed")
		}
		cur = cur.Add(2 * time.Minute) // advance past window
		if ok, _, _ := l.Allow(ctx, "k", time.Minute, 1); !ok {
			t.Fatal("hit after window reset denied")
		}
	})

	t.Run("keys are independent", func(t *testing.T) {
		now := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
		l := &InMemoryRateLimiter{now: func() time.Time { return now }, state: map[string]*window{}}
		if ok, _, _ := l.Allow(ctx, "a", time.Minute, 1); !ok {
			t.Fatal("key a first hit denied")
		}
		if ok, _, _ := l.Allow(ctx, "b", time.Minute, 1); !ok {
			t.Fatal("key b first hit denied — keys leaked")
		}
	})
}
