package webchat_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pericles-luz/crm/internal/adapter/channels/webchat"
)

func TestEnvFeatureFlag_NilGetenv(t *testing.T) {
	f := webchat.NewEnvFeatureFlag(nil)
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil || on {
		t.Errorf("want false,nil; got %v,%v", on, err)
	}
}

func TestEnvFeatureFlag_GlobalOff(t *testing.T) {
	f := webchat.NewEnvFeatureFlag(func(k string) string { return "" })
	on, _ := f.Enabled(context.Background(), uuid.New())
	if on {
		t.Error("want false when FEATURE_WEBCHAT_ENABLED is not '1'")
	}
}

func TestEnvFeatureFlag_GlobalOnNoAllowlist(t *testing.T) {
	f := webchat.NewEnvFeatureFlag(func(k string) string {
		if k == webchat.EnvEnabled {
			return "1"
		}
		return ""
	})
	on, _ := f.Enabled(context.Background(), uuid.New())
	if !on {
		t.Error("want true when enabled=1 and allowlist empty")
	}
}

func TestEnvFeatureFlag_AllowlistHit(t *testing.T) {
	id := uuid.New()
	f := webchat.NewEnvFeatureFlag(func(k string) string {
		switch k {
		case webchat.EnvEnabled:
			return "1"
		case webchat.EnvTenantAllow:
			return id.String()
		}
		return ""
	})
	on, _ := f.Enabled(context.Background(), id)
	if !on {
		t.Error("want true for listed tenant")
	}
	on, _ = f.Enabled(context.Background(), uuid.New())
	if on {
		t.Error("want false for unlisted tenant")
	}
}

func TestEnvFeatureFlag_InvalidUUIDSkipped(t *testing.T) {
	f := webchat.NewEnvFeatureFlag(func(k string) string {
		switch k {
		case webchat.EnvEnabled:
			return "1"
		case webchat.EnvTenantAllow:
			return "not-a-uuid"
		}
		return ""
	})
	// Invalid UUID skipped → allowlist stays empty → all tenants allowed
	on, _ := f.Enabled(context.Background(), uuid.New())
	if !on {
		t.Error("want true when all allowlist entries are invalid (treated as empty allowlist)")
	}
}
