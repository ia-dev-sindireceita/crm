package instagram_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
)

func TestConfigFromEnv_RequiresAppSecret(t *testing.T) {
	t.Parallel()
	_, err := instagram.ConfigFromEnv(func(s string) string {
		if s == instagram.EnvVerifyToken {
			return "ok"
		}
		return ""
	})
	if err == nil {
		t.Fatal("expected error when META_APP_SECRET is empty")
	}
}

func TestConfigFromEnv_RequiresVerifyToken(t *testing.T) {
	t.Parallel()
	_, err := instagram.ConfigFromEnv(func(s string) string {
		if s == instagram.EnvAppSecret {
			return "secret"
		}
		return ""
	})
	if err == nil {
		t.Fatal("expected error when META_INSTAGRAM_VERIFY_TOKEN is empty")
	}
}

func TestConfigFromEnv_RejectsNilGetenv(t *testing.T) {
	t.Parallel()
	if _, err := instagram.ConfigFromEnv(nil); err == nil {
		t.Fatal("expected error for nil getenv")
	}
}

func TestConfigFromEnv_PopulatesDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := instagram.ConfigFromEnv(func(s string) string {
		switch s {
		case instagram.EnvAppSecret:
			return "secret"
		case instagram.EnvVerifyToken:
			return "verify"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.AppSecret != "secret" || cfg.VerifyToken != "verify" {
		t.Errorf("unexpected secret/verify values: %+v", cfg)
	}
	if cfg.RateMaxPerMin == 0 || cfg.MaxBodyBytes == 0 || cfg.PastWindow == 0 {
		t.Errorf("defaults not populated: %+v", cfg)
	}
	if cfg.OutboundWindow == 0 {
		t.Errorf("OutboundWindow default missing: %+v", cfg)
	}
}

func TestEnvFeatureFlag_OffWhenGlobalUnset(t *testing.T) {
	t.Parallel()
	f := instagram.NewEnvFeatureFlag(func(string) string { return "" })
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Enabled: %v", err)
	}
	if on {
		t.Fatal("expected flag off when global unset")
	}
}

func TestEnvFeatureFlag_AllTenantsWhenGlobalOnAndEmptyAllowlist(t *testing.T) {
	t.Parallel()
	f := instagram.NewEnvFeatureFlag(func(s string) string {
		if s == instagram.EnvInstagramEnabled {
			return "1"
		}
		return ""
	})
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Enabled: %v", err)
	}
	if !on {
		t.Fatal("expected flag on for any tenant when allowlist is empty")
	}
}

func TestEnvFeatureFlag_RespectsAllowlist(t *testing.T) {
	t.Parallel()
	listed := uuid.New()
	other := uuid.New()
	f := instagram.NewEnvFeatureFlag(func(s string) string {
		switch s {
		case instagram.EnvInstagramEnabled:
			return "1"
		case instagram.EnvInstagramTenantAllow:
			return listed.String() + " , not-a-uuid"
		}
		return ""
	})
	on, _ := f.Enabled(context.Background(), listed)
	if !on {
		t.Error("listed tenant should be enabled")
	}
	on, _ = f.Enabled(context.Background(), other)
	if on {
		t.Error("non-listed tenant should be off")
	}
}

func TestEnvFeatureFlag_NilGetenv(t *testing.T) {
	t.Parallel()
	f := instagram.NewEnvFeatureFlag(nil)
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil || on {
		t.Fatalf("nil-getenv flag should be (false, nil), got (%v, %v)", on, err)
	}
}

func TestEnvFeatureFlag_NilReceiverFailsClosed(t *testing.T) {
	t.Parallel()
	var f *instagram.EnvFeatureFlag
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil || on {
		t.Fatalf("nil receiver should be (false, nil), got (%v, %v)", on, err)
	}
}
