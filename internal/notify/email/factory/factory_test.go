package factory_test

import (
	"errors"
	"testing"

	"github.com/pericles-luz/crm/internal/notify/email"
	"github.com/pericles-luz/crm/internal/notify/email/factory"
)

func TestFactory_DefaultIsNoopInDev(t *testing.T) {
	t.Parallel()
	s, err := factory.New(factory.MapEnv{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var _ email.EmailSender = s
}

func TestFactory_ProductionWithoutProviderFails(t *testing.T) {
	t.Parallel()
	_, err := factory.New(factory.MapEnv{"APP_ENV": "production"})
	if !errors.Is(err, factory.ErrMissingEnv) {
		t.Fatalf("New err = %v, want ErrMissingEnv", err)
	}
}

func TestFactory_NoopExplicit(t *testing.T) {
	t.Parallel()
	s, err := factory.New(factory.MapEnv{"EMAIL_PROVIDER": "noop"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s == nil {
		t.Fatal("nil sender")
	}
}

func TestFactory_Recorder(t *testing.T) {
	t.Parallel()
	s, err := factory.New(factory.MapEnv{"EMAIL_PROVIDER": "recorder"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s == nil {
		t.Fatal("nil sender")
	}
}

func TestFactory_MailgunHappyPath(t *testing.T) {
	t.Parallel()
	s, err := factory.New(factory.MapEnv{
		"EMAIL_PROVIDER":  "mailgun",
		"MAILGUN_API_KEY": "secret",
		"MAILGUN_DOMAIN":  "mg.acme.com",
		"MAILGUN_REGION":  "us",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s == nil {
		t.Fatal("nil sender")
	}
}

func TestFactory_MailgunMissingFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  factory.MapEnv
		want string
	}{
		{
			name: "no api key",
			env:  factory.MapEnv{"EMAIL_PROVIDER": "mailgun", "MAILGUN_DOMAIN": "d", "MAILGUN_REGION": "us"},
			want: "MAILGUN_API_KEY",
		},
		{
			name: "no domain",
			env:  factory.MapEnv{"EMAIL_PROVIDER": "mailgun", "MAILGUN_API_KEY": "k", "MAILGUN_REGION": "us"},
			want: "MAILGUN_DOMAIN",
		},
		{
			name: "no region",
			env:  factory.MapEnv{"EMAIL_PROVIDER": "mailgun", "MAILGUN_API_KEY": "k", "MAILGUN_DOMAIN": "d"},
			want: "MAILGUN_REGION",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := factory.New(tc.env)
			if !errors.Is(err, factory.ErrMissingEnv) {
				t.Fatalf("err = %v, want ErrMissingEnv", err)
			}
			if !contains(err.Error(), tc.want) {
				t.Fatalf("err %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

func TestFactory_MailgunBadRegion(t *testing.T) {
	t.Parallel()
	_, err := factory.New(factory.MapEnv{
		"EMAIL_PROVIDER":  "mailgun",
		"MAILGUN_API_KEY": "k",
		"MAILGUN_DOMAIN":  "d",
		"MAILGUN_REGION":  "antarctica",
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFactory_UnknownProvider(t *testing.T) {
	t.Parallel()
	_, err := factory.New(factory.MapEnv{"EMAIL_PROVIDER": "smoke-signal"})
	if !errors.Is(err, factory.ErrUnknownProvider) {
		t.Fatalf("err = %v, want ErrUnknownProvider", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
