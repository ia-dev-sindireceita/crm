package messenger_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/messenger"
)

func validCfg() messenger.Config {
	return messenger.Config{
		AppSecret:      "secret",
		VerifyToken:    "verify",
		MaxBodyBytes:   1 << 20,
		PastWindow:     time.Hour,
		FutureSkew:     time.Minute,
		DeliverTimeout: time.Second,
	}
}

func TestNew_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	f := newFakeFlag(true)

	cases := map[string]func() error{
		"empty_secret": func() error {
			c := validCfg()
			c.AppSecret = ""
			_, err := messenger.New(c, in, r, f)
			return err
		},
		"empty_verify": func() error {
			c := validCfg()
			c.VerifyToken = ""
			_, err := messenger.New(c, in, r, f)
			return err
		},
		"nil_inbox": func() error {
			_, err := messenger.New(validCfg(), nil, r, f)
			return err
		},
		"nil_resolver": func() error {
			_, err := messenger.New(validCfg(), in, nil, f)
			return err
		},
		"nil_flag": func() error {
			_, err := messenger.New(validCfg(), in, r, nil)
			return err
		},
	}
	for name, fn := range cases {
		name, fn := name, fn
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := fn(); err == nil {
				t.Errorf("%s: expected error", name)
			}
		})
	}
}

func TestNew_AppliesOptions(t *testing.T) {
	t.Parallel()
	c := newFakeClock(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	a, err := messenger.New(
		validCfg(),
		newFakeInbox(), newFakeResolver(), newFakeFlag(true),
		messenger.WithClock(c),
		messenger.WithLogger(nil), // nil logger option is a no-op
		messenger.WithClock(nil),  // nil clock option is a no-op
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a == nil {
		t.Fatal("adapter is nil")
	}
}

func TestTenantResolverFunc_DelegatesToClosure(t *testing.T) {
	t.Parallel()
	want := uuid.New()
	resolver := messenger.TenantResolverFunc(func(_ context.Context, _ string) (uuid.UUID, error) {
		return want, nil
	})
	got, err := resolver.Resolve(context.Background(), "page-42")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != want {
		t.Errorf("got %s want %s", got, want)
	}
}
