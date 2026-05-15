package whatsapp_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
)

func TestConfigFromEnv_OK(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		whatsapp.EnvAppSecret:   "s",
		whatsapp.EnvVerifyToken: "v",
	}
	cfg, err := whatsapp.ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppSecret != "s" || cfg.VerifyToken != "v" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.RateMaxPerMin <= 0 || cfg.MaxBodyBytes <= 0 {
		t.Fatalf("defaults missing: %+v", cfg)
	}
}

func TestConfigFromEnv_NilGetenv(t *testing.T) {
	t.Parallel()
	if _, err := whatsapp.ConfigFromEnv(nil); err == nil {
		t.Fatal("expected error for nil getenv")
	}
}

func TestConfigFromEnv_MissingAppSecret(t *testing.T) {
	t.Parallel()
	if _, err := whatsapp.ConfigFromEnv(func(k string) string {
		if k == whatsapp.EnvVerifyToken {
			return "v"
		}
		return ""
	}); err == nil {
		t.Fatal("expected error")
	}
}

func TestConfigFromEnv_MissingVerifyToken(t *testing.T) {
	t.Parallel()
	if _, err := whatsapp.ConfigFromEnv(func(k string) string {
		if k == whatsapp.EnvAppSecret {
			return "s"
		}
		return ""
	}); err == nil {
		t.Fatal("expected error")
	}
}

func TestEnvFeatureFlag_GlobalOff(t *testing.T) {
	t.Parallel()
	f := whatsapp.NewEnvFeatureFlag(func(string) string { return "" })
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if on {
		t.Fatal("default should be off")
	}
}

func TestEnvFeatureFlag_GlobalOn_EmptyAllowlist_AllOn(t *testing.T) {
	t.Parallel()
	f := whatsapp.NewEnvFeatureFlag(func(k string) string {
		if k == whatsapp.EnvWhatsAppEnabled {
			return "1"
		}
		return ""
	})
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if !on {
		t.Fatal("globalOn + empty allowlist should be on for any tenant")
	}
}

func TestEnvFeatureFlag_GlobalOn_AllowlistMatches(t *testing.T) {
	t.Parallel()
	allowed := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	other := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	f := whatsapp.NewEnvFeatureFlag(func(k string) string {
		switch k {
		case whatsapp.EnvWhatsAppEnabled:
			return "1"
		case whatsapp.EnvWhatsAppTenantAllow:
			return allowed.String() + ",not-a-uuid"
		}
		return ""
	})
	if on, _ := f.Enabled(context.Background(), allowed); !on {
		t.Fatal("allowed tenant should be on")
	}
	if on, _ := f.Enabled(context.Background(), other); on {
		t.Fatal("non-allowed tenant should be off")
	}
}

func TestEnvFeatureFlag_NilGetenv(t *testing.T) {
	t.Parallel()
	f := whatsapp.NewEnvFeatureFlag(nil)
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if on {
		t.Fatal("nil getenv should yield off")
	}
}

func TestEnvFeatureFlag_NilReceiverOff(t *testing.T) {
	t.Parallel()
	var f *whatsapp.EnvFeatureFlag
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if on {
		t.Fatal("nil flag pointer must be off")
	}
}
