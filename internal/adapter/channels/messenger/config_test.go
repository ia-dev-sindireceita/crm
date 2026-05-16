package messenger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/messenger"
)

func envGetter(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConfigFromEnv_LoadsAllFields(t *testing.T) {
	t.Parallel()
	cfg, err := messenger.ConfigFromEnv(envGetter(map[string]string{
		messenger.EnvAppSecret:   "secret",
		messenger.EnvVerifyToken: "verify",
	}))
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.AppSecret != "secret" || cfg.VerifyToken != "verify" {
		t.Errorf("secret/verify mismatch: %+v", cfg)
	}
	if cfg.MaxBodyBytes <= 0 || cfg.PastWindow <= 0 || cfg.FutureSkew <= 0 || cfg.DeliverTimeout <= 0 {
		t.Errorf("zero default: %+v", cfg)
	}
}

func TestConfigFromEnv_NilGetenv(t *testing.T) {
	t.Parallel()
	if _, err := messenger.ConfigFromEnv(nil); err == nil {
		t.Fatal("expected error on nil getenv")
	}
}

func TestConfigFromEnv_RequiresSecrets(t *testing.T) {
	t.Parallel()
	cases := []map[string]string{
		{messenger.EnvVerifyToken: "v"},                               // missing app secret
		{messenger.EnvAppSecret: "  ", messenger.EnvVerifyToken: "v"}, // whitespace-only secret
		{messenger.EnvAppSecret: "s"},                                 // missing verify token
		{messenger.EnvAppSecret: "s", messenger.EnvVerifyToken: ""},   // empty verify
	}
	for i, env := range cases {
		_, err := messenger.ConfigFromEnv(envGetter(env))
		if err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestEnvFeatureFlag_DefaultOff(t *testing.T) {
	t.Parallel()
	f := messenger.NewEnvFeatureFlag(envGetter(map[string]string{}))
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil || on {
		t.Fatalf("default expected off,nil; got %v,%v", on, err)
	}
}

func TestEnvFeatureFlag_NilGetenvSafe(t *testing.T) {
	t.Parallel()
	f := messenger.NewEnvFeatureFlag(nil)
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil || on {
		t.Fatalf("nil getenv should yield off,nil; got %v,%v", on, err)
	}
}

func TestEnvFeatureFlag_NilReceiverSafe(t *testing.T) {
	t.Parallel()
	var f *messenger.EnvFeatureFlag
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil || on {
		t.Fatalf("nil receiver should yield off,nil; got %v,%v", on, err)
	}
}

func TestEnvFeatureFlag_GlobalOnEmptyAllowlist(t *testing.T) {
	t.Parallel()
	f := messenger.NewEnvFeatureFlag(envGetter(map[string]string{
		messenger.EnvMessengerEnabled: "1",
	}))
	on, err := f.Enabled(context.Background(), uuid.New())
	if err != nil || !on {
		t.Fatalf("empty allowlist with globalOn should enable all; got %v,%v", on, err)
	}
}

func TestEnvFeatureFlag_AllowlistFiltering(t *testing.T) {
	t.Parallel()
	allowed := uuid.New()
	denied := uuid.New()
	f := messenger.NewEnvFeatureFlag(envGetter(map[string]string{
		messenger.EnvMessengerEnabled:     "1",
		messenger.EnvMessengerTenantAllow: allowed.String() + ",not-a-uuid,",
	}))
	on, _ := f.Enabled(context.Background(), allowed)
	if !on {
		t.Errorf("allowed tenant should be enabled")
	}
	off, _ := f.Enabled(context.Background(), denied)
	if off {
		t.Errorf("denied tenant should be off")
	}
}

func TestEnvFeatureFlag_TrimsAllowlistEntries(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	spaced := "  " + id.String() + "  "
	f := messenger.NewEnvFeatureFlag(envGetter(map[string]string{
		messenger.EnvMessengerEnabled:     "1",
		messenger.EnvMessengerTenantAllow: strings.Join([]string{spaced}, ","),
	}))
	on, _ := f.Enabled(context.Background(), id)
	if !on {
		t.Errorf("trimmed tenant should be enabled")
	}
}
